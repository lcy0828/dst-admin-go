package httpapi

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLargeUploadUsesDataVolumeAndAcceptsTrailingFields(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(root, "unavailable-tmp"))
	input, output := io.Pipe()
	writer := multipart.NewWriter(output)
	done := make(chan error, 1)
	go func() {
		part, err := writer.CreateFormFile("file", "save.zip")
		if err == nil {
			block := make([]byte, 1<<20)
			for i := 0; i < 70 && err == nil; i++ {
				_, err = part.Write(block)
			}
		}
		if err == nil {
			err = writer.WriteField("name", "after-file")
		}
		if err == nil {
			err = writer.Close()
		}
		_ = output.CloseWithError(err)
		done <- err
	}()
	defer input.Close()
	request := httptest.NewRequest(http.MethodPost, "/upload", input)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = request
	file, err := readUpload(c, root, 80<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if file.Size != 70<<20 || file.Fields["name"] != "after-file" || filepath.Dir(file.Name()) != filepath.Join(root, ".uploads") {
		t.Fatalf("bad upload: %#v", file)
	}
	name := file.Name()
	file.cleanup()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("temporary upload not removed")
	}
}

func TestUploadRejectsOversizeAndCleansPartialFile(t *testing.T) {
	root := t.TempDir()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "save.zip")
	_, _ = io.WriteString(part, strings.Repeat("x", 1025))
	_ = writer.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/upload", &body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	_, err := readUpload(c, root, 1024)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, ".uploads", "upload-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("partial files remain: %v %v", files, err)
	}
}

func TestUploadFailureReportsTimeoutAndFullDisk(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		status int
		code   string
	}{
		{os.ErrDeadlineExceeded, http.StatusRequestTimeout, "UPLOAD_TIMEOUT"},
		{&os.PathError{Op: "write", Path: "private-path", Err: syscall.ENOSPC}, http.StatusInsufficientStorage, "UPLOAD_STORAGE_FULL"},
	} {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		uploadFailure(c, tc.cause)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.code) || strings.Contains(response.Body.String(), "private-path") {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
}
