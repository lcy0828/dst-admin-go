package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapServesAPIAssetsAndSPAFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<main>app</main>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("window.app=true"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusTeapot)
		_, _ = response.Write([]byte(request.URL.Path))
	})
	handler, err := Wrap(api, root)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		path       string
		status     int
		body       string
		cacheMatch string
	}{
		{path: "/api/v2/auth/session", status: http.StatusTeapot, body: "/api/v2/auth/session"},
		{path: "/agent", status: http.StatusTeapot, body: "/agent"},
		{path: "/assets/app.js", status: http.StatusOK, body: "window.app=true", cacheMatch: "immutable"},
		{path: "/servers/commands", status: http.StatusOK, body: "<main>app</main>", cacheMatch: "no-cache"},
		{path: "/assets/missing.js", status: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if test.cacheMatch != "" && !strings.Contains(response.Header().Get("Cache-Control"), test.cacheMatch) {
				t.Fatalf("cache-control=%q", response.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestWrapRejectsInvalidRoot(t *testing.T) {
	if _, err := Wrap(http.NotFoundHandler(), t.TempDir()); err == nil {
		t.Fatal("expected missing index error")
	}
}
