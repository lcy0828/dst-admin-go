package aicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"dont/internal/apicontract"
	"github.com/google/uuid"
)

const discoverAPI = "find_api"

func (s *Service) InitialTools() []Tool {
	list := Tools()
	if s.catalog != nil {
		list = append(list, Tool{Name: discoverAPI, Description: "Discover all management APIs by domain or operationId, then load their typed tools. 支持中文需求、故障现象、英文关键词或精确 operationId。Search Chinese or English purposes and symptoms; refine ambiguous searches. Use this whenever a requested action is not in the initial legacy tools; do not conclude it is unavailable. All administrator UI management APIs are included. Prefer configureRoomBackupSchedule to enable/change/pause automatic backups.", InputSchema: schema([]string{}, map[string]any{"includeResultSchema": map[string]any{"type": "boolean", "description": "Get complete business result schemas for one exact operationId/api_ tool name. Also includes inputSchema and receipt outputSchema. Defaults to false."}, "includeSchema": map[string]any{"type": "boolean", "description": "Return inputSchema and receipt outputSchema; defaults to false. Detailed business fields use includeResultSchema with an exact operationId."}, "tags": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"}}, "access": map[string]any{"type": "string", "enum": []string{"read", "write"}}, "risk": map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}}, "runtime": map[string]any{"type": "string", "enum": []string{"any", "server-running", "shard-ready"}}, "query": map[string]any{"type": "string", "maxLength": 256}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 12}})})
	}
	return list
}

func apiTool(op *apicontract.Operation) (Tool, error) {
	input, err := op.AIInputSchema()
	if err != nil {
		return Tool{}, err
	}
	description := op.Summary
	if semantic := op.Semantics(); semantic != nil {
		description = semantic.Purpose + "\n" + description
	}
	if op.Description != "" {
		description += ". " + op.Description
	}
	if op.Deprecated {
		description += ". Compatibility operation (deprecated)."
		if op.ReplacedBy != "" {
			description += " Prefer api_" + op.ReplacedBy + " for the same behavior."
		}
	}
	if op.Mutation() {
		description += ". Executes the administrator's requested operation directly. Preserve unspecified fields and revisions. Submitted tasks require checking their actual result."
	}
	if op.Transport == "multipart" {
		description += ". File fields use attachmentId references from uploaded files."
	}
	if hint := activityArgumentHint(op.ID); hint != "" {
		description += "\nArguments: " + hint
	}
	metadata := op.ToolMetadata()
	description += "\n" + op.ResultSummary()
	return Tool{Name: op.ToolName(), Description: description, InputSchema: input, Mutation: op.Mutation(), Metadata: &metadata}, nil
}

func (s *Service) AvailableTools() ([]Tool, error) {
	list := s.InitialTools()
	if s.catalog != nil {
		for _, op := range s.catalog.Operations() {
			if !op.Callable() {
				continue
			}
			tool, err := apiTool(op)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", op.ID, err)
			}
			list = append(list, tool)
		}
	}
	return list, nil
}

func (s *Service) tool(name string) (Tool, bool) {
	for _, tool := range s.InitialTools() {
		if tool.Name == name {
			return tool, true
		}
	}
	if s.catalog != nil && strings.HasPrefix(name, apicontract.ToolPrefix) {
		if op, ok := s.catalog.Operation(name); ok && op.Callable() {
			tool, err := apiTool(op)
			return tool, err == nil
		}
	}
	return Tool{}, false
}

func (s *Service) modelTools(loaded map[string]Tool) []any {
	list := s.InitialTools()
	names := make([]string, 0, len(loaded))
	for name := range loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		list = append(list, loaded[name])
	}
	result := make([]any, 0, len(list))
	for _, tool := range list {
		result = append(result, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
	}
	return result
}
func (s *Service) loadDiscovered(data json.RawMessage, loaded map[string]Tool) {
	var result struct {
		Items []struct {
			Tool string `json:"tool"`
		} `json:"items"`
	}
	if json.Unmarshal(data, &result) != nil {
		return
	}
	for _, item := range result.Items {
		if tool, ok := s.tool(item.Tool); ok {
			loaded[tool.Name] = tool
		}
	}
}

func (s *Service) discover(raw json.RawMessage) (json.RawMessage, error) {
	return s.discoverFor(Principal{}, raw)
}
func (s *Service) discoverFor(p Principal, raw json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Query               string `json:"query"`
		Limit               int    `json:"limit"`
		IncludeSchema       bool   `json:"includeSchema"`
		IncludeResultSchema bool   `json:"includeResultSchema"`
		apicontract.MetadataFilter
	}
	if json.Unmarshal(raw, &in) != nil || in.MetadataFilter.Validate() != nil || utf8.RuneCountInString(in.Query) > 256 || in.Limit < 0 || in.Limit > 12 {
		return nil, ErrInvalid
	}
	if in.Limit == 0 {
		in.Limit = 8
	}
	if in.IncludeResultSchema {
		if op, ok := s.catalog.Operation(strings.TrimSpace(in.Query)); !ok || !op.Callable() {
			return nil, fmt.Errorf("%w: includeResultSchema requires an exact operationId or api_ tool name", ErrInvalid)
		}
		in.Limit = 1
	}
	query := in.Query
	items := []map[string]any{}
	for _, op := range s.catalog.SearchFiltered(query, len(s.catalog.Operations()), in.MetadataFilter) {
		if !p.AllowsTool(op.ToolName()) {
			continue
		}
		metadata := op.ToolMetadata()
		item := map[string]any{"operationId": op.ID, "tool": op.ToolName(), "summary": op.Summary, "tags": metadata.Tags, "access": metadata.Access, "risk": metadata.Risk, "runtime": metadata.Runtime, "transport": op.Transport, "deprecated": op.Deprecated, "replacedBy": op.ReplacedBy}
		if semantic := op.Semantics(); semantic != nil {
			item["purpose"] = semantic.Purpose
		}
		item["resultSummary"] = op.ResultSummary()
		if hint := activityArgumentHint(op.ID); hint != "" {
			item["argumentHint"] = hint
		}
		if in.IncludeSchema || in.IncludeResultSchema {
			if op.Description != "" {
				item["description"] = op.Description
			}
			input, err := op.AIInputSchema()
			if err != nil {
				return nil, err
			}
			item["inputSchema"] = input
			item["outputSchema"] = receiptOutputSchema()
		}
		if in.IncludeResultSchema {
			contract, err := op.ResultSchema()
			if err != nil {
				return nil, err
			}
			item["resultContract"] = contract
		}
		items = append(items, item)
		if len(items) >= in.Limit {
			break
		}
	}
	encoded, err := json.Marshal(map[string]any{"items": items, "count": len(items), "instruction": "Matching typed tools are now loaded in the page assistant. External MCP clients can use call_api with operationId and arguments, or the listed api_ tool. Use includeSchema=true for arguments; includeResultSchema=true with an exact operationId for business result fields. Discover more specific keywords if needed. An empty search is not proof a capability is absent."})
	if len(encoded) > 60<<10 {
		return nil, fmt.Errorf("%w: discovery schemas exceed the response limit; request fewer tools or use the published OpenAPI contract", ErrInvalid)
	}
	return encoded, err
}

func (s *Service) invokeAPI(ctx context.Context, p Principal, name string, raw json.RawMessage) (Invocation, error) {
	if len(raw) > 64<<10 {
		return Invocation{}, ErrInvalid
	}
	if !s.Enabled() || p.ID == "" || !p.AllowsTool(name) {
		return Invocation{}, ErrDenied
	}
	var op *apicontract.Operation
	var in apicontract.Input
	var err error
	mutation := false
	var privateValues []string
	if name != discoverAPI {
		var ok bool
		op, ok = s.catalog.Operation(name)
		if !ok || !op.Callable() {
			return Invocation{}, ErrDenied
		}
		resolved, values, resolveErr := s.Store.resolvePrivateInputs(p, raw)
		if resolveErr != nil {
			return Invocation{}, resolveErr
		}
		privateValues = values
		in, err = op.Validate(resolved)
		if err != nil {
			if len(values) > 0 {
				return Invocation{}, fmt.Errorf("%w: private input does not match the requested API field", ErrInvalid)
			}
			if hint := activityArgumentHint(op.ID); hint != "" {
				return Invocation{}, fmt.Errorf("%w: %s: %s. %s", ErrInvalid, op.ID, activityArgumentProblem(resolved, op.ID), hint)
			}
			return Invocation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		privateValues = append(privateValues, op.SensitiveValues(resolved)...)
		mutation = op.Mutation()
		if !p.AllowsAPI(op, in) {
			return Invocation{}, ErrDenied
		}
		if p.RoomID != "" && op.ID != "listBotCapabilities" && op.ID != "listRooms" {
			room, ok := in.Path["roomId"].(string)
			if !ok || !allowed(p, room) {
				return Invocation{}, ErrDenied
			}
		}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return Invocation{}, ErrInvalid
	}
	encoded, _ := json.Marshal(value)
	recorded := encoded
	if op != nil {
		recorded = op.RedactInput(encoded)
	}
	call := Call{ID: uuid.NewString(), Source: p.ID, Tool: name, Arguments: string(Sanitize(recorded)), Status: "executing", CreatedAt: time.Now().UTC()}
	if room, ok := in.Path["roomId"].(string); ok {
		call.RoomID = room
	}
	s.Store.mu.Lock()
	if mutation {
		call.CallKey = digest(p.ID + "\x00" + in.RequestID)
		call.Fingerprint = digest(name + "\x00" + string(encoded))
		var old Call
		result := s.Store.db.Table(s.Store.callTable).Where("call_key = ?", call.CallKey).First(&old)
		if result.Error == nil {
			s.Store.mu.Unlock()
			if old.Fingerprint != call.Fingerprint {
				return Invocation{}, ErrConflict
			}
			return invocation(old), nil
		}
		if !result.RecordNotFound() {
			s.Store.mu.Unlock()
			return Invocation{}, result.Error
		}
	} else {
		call.CallKey = call.ID
	}
	select {
	case s.toolSlots <- struct{}{}:
		defer func() { <-s.toolSlots }()
	default:
		s.Store.mu.Unlock()
		return Invocation{}, ErrBusy
	}
	err = s.Store.prune()
	if err == nil {
		err = s.Store.db.Table(s.Store.callTable).Create(&call).Error
	}
	s.Store.mu.Unlock()
	if err != nil {
		return Invocation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var data json.RawMessage
	if name == discoverAPI {
		data, err = s.discoverFor(p, raw)
	} else if backend, ok := s.backend.(APIBackend); ok {
		data, err = backend.InvokeAPI(ctx, p, op, in)
		if err == nil && op.ID == "listRooms" {
			data, err = filterAPIList(p, data)
		}
	} else {
		err = ErrUnavailable
	}
	call.Status = "succeeded"
	if err != nil {
		call.Status = "failed"
		if mutation && (errors.Is(err, ErrUncertain) || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			call.Status = "uncertain"
		}
		data, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	if name != discoverAPI || err != nil {
		// Discovery contains only bundled contract text, never settings or
		// caller input. Redacting schema property names corrupts that contract.
		// All actual management results still use the usual redaction path.
		data = Sanitize(redactPrivateValues(data, privateValues))
	}
	data = boundResult(data, 64<<10)
	call.Result = string(data)
	if err := s.Store.db.Table(s.Store.callTable).Where("id = ? AND status = ?", call.ID, "executing").Updates(map[string]any{"status": call.Status, "result": call.Result}).Error; err != nil {
		return Invocation{CallID: call.ID, Status: "uncertain", Data: data}, fmt.Errorf("AI_RESULT_PERSISTENCE_FAILED: inspect task before retry")
	}
	return invocation(call), nil
}

type requestMetadataKey struct{}
type RequestMetadata struct {
	RemoteAddr, Host, ClientIP string
	Credentials                http.Header
}

func RequestAccess(r *http.Request, clientIP string) *RequestMetadata {
	credentials := http.Header{}
	for _, name := range []string{"Cookie", "Authorization", "X-CSRF-Token"} {
		if value := r.Header.Get(name); value != "" {
			credentials.Set(name, value)
		}
	}
	return &RequestMetadata{RemoteAddr: r.RemoteAddr, Host: r.Host, ClientIP: clientIP, Credentials: credentials}
}

func WithRequestMetadata(ctx context.Context, r *RequestMetadata) context.Context {
	return context.WithValue(ctx, requestMetadataKey{}, r)
}
func Metadata(ctx context.Context) *RequestMetadata {
	value, _ := ctx.Value(requestMetadataKey{}).(*RequestMetadata)
	return value
}
