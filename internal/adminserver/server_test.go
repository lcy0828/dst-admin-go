package adminserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUploadDeadlineExtendsBodyReadButNotOrdinaryRequests(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "timeout", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(uploadReadTimeout(handler, 2*time.Second))
	server.Config.ReadTimeout = 100 * time.Millisecond
	server.Config.ReadHeaderTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()
	for _, path := range []string{"/api/v2/save-imports/upload", "/api/v2/auth/login"} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+path, &delayedBody{reader: strings.NewReader("valid body")})
		request.ContentLength = 10
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		expected := http.StatusNoContent
		if path == "/api/v2/auth/login" {
			expected = http.StatusRequestTimeout
		}
		if response.StatusCode != expected {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
	}
}

type delayedBody struct {
	reader  io.Reader
	delayed bool
}

func (r *delayedBody) Read(p []byte) (int, error) {
	if !r.delayed {
		time.Sleep(250 * time.Millisecond)
		r.delayed = true
	}
	return r.reader.Read(p)
}
