package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
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
		{path: "/", status: http.StatusOK, body: "<main>app</main>", cacheMatch: "no-cache"},
		{path: "/index.html", status: http.StatusOK, body: "<main>app</main>", cacheMatch: "no-cache"},
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

func TestFilesystemSPAHandlesHeadAndPreservesAPIRouting(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":    {Data: []byte("<main>embedded</main>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler, err := wrapFS(api, assets)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/setup", http.StatusOK},
		{http.MethodHead, "/rooms/settings", http.StatusOK},
		{http.MethodGet, "/missing.js", http.StatusNotFound},
		{http.MethodGet, "/assets/missing", http.StatusNotFound},
		{http.MethodGet, "/api/v2/auth/session", http.StatusTeapot},
		{http.MethodGet, "/agent/connect", http.StatusTeapot},
		{http.MethodPost, "/setup", http.StatusTeapot},
	} {
		t.Run(item.method+item.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(item.method, item.path, nil))
			if response.Code != item.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if item.method == http.MethodHead && response.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
		})
	}
}

func TestReleaseServesEmbeddedFrontendWithoutExternalDirectory(t *testing.T) {
	if !Embedded() {
		t.Skip("release assets are prepared by the packaging workflow")
	}
	handler, err := Wrap(http.NotFoundHandler(), "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "/assets/") {
		t.Fatalf("embedded frontend is unavailable: status=%d", response.Code)
	}
}

func TestWrapRejectsInvalidRoot(t *testing.T) {
	if _, err := Wrap(http.NotFoundHandler(), t.TempDir()); err == nil {
		t.Fatal("expected missing index error")
	}
}
