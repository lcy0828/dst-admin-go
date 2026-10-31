package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"github.com/google/uuid"
)

// Dispatch uses the original verified request's credentials and the real router.
// No listener, fabricated session or authentication bypass is involved.
func (b *AIBackend) ConfigureAPI(handler http.Handler, files *aicontrol.Store) {
	b.apiRouter = handler
	b.apiFiles = files
}

func (b *AIBackend) InvokeAPI(ctx context.Context, p aicontrol.Principal, op *apicontract.Operation, in apicontract.Input) (json.RawMessage, error) {
	if b.apiRouter == nil {
		return nil, aicontrol.ErrUnavailable
	}
	uri, err := op.URL(in)
	if err != nil {
		return nil, err
	}
	if op.Transport == "download" || op.Transport == "stream" {
		if b.apiFiles == nil || aicontrol.Metadata(ctx) == nil {
			return nil, aicontrol.ErrUnavailable
		}
		resource, e := b.apiFiles.CreateResource(p, op, in)
		if e != nil {
			return nil, e
		}
		return json.Marshal(map[string]any{"resource": map[string]any{"url": "/api/v2/system/ai/resources/" + resource.ID, "name": op.Summary, "kind": op.Transport, "authenticated": true, "fetched": false, "expiresAt": resource.CreatedAt.Add(24 * time.Hour)}, "message": "Authenticated resource link prepared; its contents have not been downloaded or inspected."})
	}
	var body io.Reader
	contentType := "application/json"
	if op.Transport == "multipart" {
		reader, kind, e := b.multipartBody(ctx, p, in.Body)
		if e != nil {
			return nil, e
		}
		defer reader.Close()
		body = reader
		contentType = kind
	} else if in.Body != nil {
		encoded, e := json.Marshal(in.Body)
		if e != nil {
			return nil, e
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, op.Method, uri, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	for key, value := range in.Headers {
		request.Header.Set(key, fmt.Sprint(value))
	}
	if in.RequestID != "" {
		request.Header.Set("Idempotency-Key", in.RequestID)
	}
	if err = b.authenticateAPIRequest(ctx, request); err != nil {
		return nil, err
	}
	writer := &aiResponseWriter{header: http.Header{}, status: 200}
	b.apiRouter.ServeHTTP(writer, request)
	if writer.overflow {
		if op.Mutation() {
			return nil, fmt.Errorf("%w: response is too large; inspect the actual task or management records before retrying", aicontrol.ErrUncertain)
		}
		return nil, fmt.Errorf("AI_RESULT_TOO_LARGE: use a narrower query or pagination")
	}
	var result struct {
		Data  json.RawMessage `json:"data"`
		Error *errorBody      `json:"error"`
	}
	if json.Unmarshal(writer.body.Bytes(), &result) != nil {
		if writer.status == 204 {
			return json.Marshal(map[string]any{"operationId": op.ID, "httpStatus": writer.status})
		}
		if op.Mutation() && writer.status < 400 {
			return nil, fmt.Errorf("%w: HTTP %d returned an unexpected response; inspect actual state before retrying", aicontrol.ErrUncertain, writer.status)
		}
		return nil, fmt.Errorf("AI_INVALID_PLATFORM_RESPONSE: HTTP %d", writer.status)
	}
	if result.Error != nil {
		detail, _ := json.Marshal(result.Error)
		return nil, &aicontrol.PlatformError{Status: writer.status, Code: result.Error.Code, Message: result.Error.Message, Payload: detail}
	}
	if writer.status >= 400 {
		return nil, fmt.Errorf("AI_PLATFORM_HTTP_%d", writer.status)
	}
	data := result.Data
	if len(data) > 0 {
		data = aicontrol.Sanitize(data)
	}
	response := map[string]any{"operationId": op.ID, "httpStatus": writer.status, "data": data}
	if p.ID != "" && b.apiFiles != nil && aicontrol.HasPrivateResult(result.Data) {
		id, err := b.apiFiles.SavePrivateResult(p, result.Data)
		if err != nil {
			response["privateResultError"] = "Operation succeeded, but private result storage is unavailable. Inspect the original management page before repeating this operation."
		} else {
			response["privateResult"] = map[string]any{"url": "/api/v2/system/ai/private-results/" + id, "kind": "download", "authenticated": true, "fetched": false}
		}
	}
	return json.Marshal(response)
}

func (b *AIBackend) authenticateAPIRequest(ctx context.Context, request *http.Request) error {
	meta := aicontrol.Metadata(ctx)
	if meta == nil || len(meta.Credentials) == 0 {
		return aicontrol.ErrDenied
	}
	{
		request.RemoteAddr = meta.RemoteAddr
		request.Host = meta.Host
		if meta.ClientIP != "" {
			request.RemoteAddr = net.JoinHostPort(meta.ClientIP, "0")
		}
		for key, values := range meta.Credentials {
			request.Header[key] = append([]string(nil), values...)
		}
	}
	if request.Host == "" {
		request.Host = "dst-admin.internal"
	}
	request.Header.Set("User-Agent", "DST Admin AI")
	request.Header.Set("X-Request-ID", "ai-api-"+uuid.NewString())
	return nil
}

func (b *AIBackend) multipartBody(ctx context.Context, p aicontrol.Principal, body any) (*io.PipeReader, string, error) {
	fields, ok := body.(map[string]any)
	if !ok || b.apiFiles == nil {
		return nil, "", aicontrol.ErrInvalid
	}
	type filePart struct {
		value aicontrol.Attachment
		file  *os.File
	}
	files := map[string]filePart{}
	closeFiles := func() {
		for _, part := range files {
			part.file.Close()
		}
	}
	keys := make([]string, 0, len(fields))
	for key, value := range fields {
		keys = append(keys, key)
		if reference, ok := value.(map[string]any); ok {
			id, _ := reference["attachmentId"].(string)
			item, file, err := b.apiFiles.OpenAttachment(p, id)
			if err != nil {
				closeFiles()
				return nil, "", err
			}
			files[key] = filePart{item, file}
		}
	}
	sort.Strings(keys)
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	stopCancel := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	go func() {
		defer stopCancel()
		defer closeFiles()
		var err error
		for _, key := range keys {
			if err = ctx.Err(); err != nil {
				break
			}
			if part, ok := files[key]; ok {
				var target io.Writer
				target, err = form.CreateFormFile(key, part.value.Name)
				if err == nil {
					_, err = io.Copy(target, part.file)
				}
			} else {
				err = form.WriteField(key, fmt.Sprint(fields[key]))
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			err = form.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader, form.FormDataContentType(), nil
}
