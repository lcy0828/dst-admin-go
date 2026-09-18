package httpapi

import (
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"

	"dont/internal/tempfiles"

	"github.com/gin-gonic/gin"
)

type uploadedFile struct {
	*tempfiles.File
	Filename string
	Size     int64
	Fields   map[string]string
}

func (f *uploadedFile) cleanup() {
	if f != nil && f.File != nil {
		_ = f.Close()
	}
}

// readUpload stages multipart files on the feature's data volume, never /tmp.
// Fields may precede or follow the file. Both memory and disk usage are bounded.
func readUpload(c *gin.Context, root string, limit int64) (result *uploadedFile, err error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+(1<<20))
	reader, err := c.Request.MultipartReader()
	if err != nil {
		return nil, err
	}
	value := &uploadedFile{Fields: make(map[string]string)}
	defer func() {
		if err != nil {
			value.cleanup()
		}
	}()
	for count := 0; ; count++ {
		part, readErr := reader.NextPart()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if count >= 16 {
			return nil, errors.New("too many upload fields")
		}
		if part.FileName() == "" {
			data, readErr := io.ReadAll(io.LimitReader(part, (8<<10)+1))
			if readErr != nil {
				return nil, readErr
			}
			if len(data) > 8<<10 {
				return nil, errors.New("upload field is too large")
			}
			value.Fields[part.FormName()] = string(data)
		} else {
			if part.FormName() != "file" || value.File != nil {
				return nil, errors.New("expected one uploaded file")
			}
			value.File, err = tempfiles.Create(c.Request.Context(), root, tempfiles.Uploads)
			if err != nil {
				return nil, err
			}
			value.Filename = part.FileName()
			value.Size, err = io.Copy(value.File, io.LimitReader(part, limit+1))
			if err != nil {
				return nil, err
			}
			if value.Size > limit {
				return nil, &http.MaxBytesError{Limit: limit}
			}
		}
		if err := part.Close(); err != nil {
			return nil, err
		}
	}
	if value.File == nil {
		return nil, http.ErrMissingFile
	}
	if _, err := value.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return value, nil
}

func uploadFailure(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	var timeout net.Error
	switch {
	case errors.As(err, &tooLarge):
		Failure(c, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", "上传文件超过大小上限", nil)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		Failure(c, http.StatusInsufficientStorage, "UPLOAD_STORAGE_FULL", "上传临时目录所在的数据盘空间不足", nil)
	case errors.As(err, &timeout) && timeout.Timeout():
		Failure(c, http.StatusRequestTimeout, "UPLOAD_TIMEOUT", "上传超时，请检查网络后重试", nil)
	case errors.Is(err, http.ErrMissingFile):
		Failure(c, http.StatusBadRequest, "UPLOAD_REQUIRED", "请选择上传文件", nil)
	default:
		Failure(c, http.StatusBadRequest, "UPLOAD_READ_FAILED", "无法读取上传文件，请检查文件与网络后重试", nil)
	}
}
