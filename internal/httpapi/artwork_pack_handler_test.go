package httpapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dont/internal/artworkpack"
	"github.com/gin-gonic/gin"
)

func TestArtworkPackControllerRoutesAndRejectedImports(t *testing.T) {
	service, err := artworkpack.New(t.TempDir(), artworkpack.OfficialRelease(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close(context.Background()) })
	router := gin.New()
	group := router.Group("/api/v2", RuntimeTargetBoundary())
	NewArtworkPackHandler(service).Register(group)
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"GET", "", "", 200},
		{"GET", "/index", "", 200},
		{"GET", "/artwork/bundle", "", 404},
		{"GET", "/entities", "", 409},
		{"GET", "/entities?offset=wrong", "", 422},
		{"POST", "/install", `{"source":"http://127.0.0.1/private"}`, 422},
		{"POST", "/cancel", `{}`, 202},
		{"DELETE", "", "", 200},
	} {
		req := httptest.NewRequest(tc.method, "/api/v2/entity-catalog/artwork-pack"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(RuntimeTargetHeader, "agent:selected")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "../../saves/world.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("invalid archive"))
	form.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/entity-catalog/artwork-pack/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 422 || service.Status().Installed != nil || service.Status().Busy {
		t.Fatal(response, service.Status())
	}
}
