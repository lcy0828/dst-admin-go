package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Wrap serves the release's embedded SPA unless an external root is explicitly
// provided. Development builds without embedded assets remain API-only.
func Wrap(api http.Handler, root string) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("API handler is required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		assets := embeddedAssets()
		if assets == nil {
			return api, nil
		}
		return wrapFS(api, assets)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("web UI root is not a directory")
	}
	return wrapFS(api, os.DirFS(absolute))
}

func wrapFS(api http.Handler, assets fs.FS) (http.Handler, error) {
	if info, err := fs.Stat(assets, "index.html"); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("web UI index.html is unavailable")
	}
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if apiRequest(request) || (request.Method != http.MethodGet && request.Method != http.MethodHead) {
			api.ServeHTTP(response, request)
			return
		}

		cleaned := path.Clean("/" + request.URL.Path)
		relative := strings.TrimPrefix(cleaned, "/")
		if relative == "" || relative == "index.html" {
			relative = "index.html"
			response.Header().Set("Cache-Control", "no-cache")
		}
		if file, statErr := fs.Stat(assets, relative); statErr == nil && file.Mode().IsRegular() {
			if strings.HasPrefix(cleaned, "/assets/") {
				response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			servePath(files, response, request, relative)
			return
		}
		if strings.HasPrefix(cleaned, "/assets/") || filepath.Ext(cleaned) != "" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Cache-Control", "no-cache")
		servePath(files, response, request, "index.html")
	}), nil
}

func servePath(files http.Handler, response http.ResponseWriter, request *http.Request, name string) {
	copy := request.Clone(request.Context())
	copy.URL.Path = "/" + name
	// FileServer redirects explicit index.html requests. Serve the root index
	// for SPA routes without redirecting the browser away from its route.
	if name == "index.html" {
		copy.URL.Path = "/"
	}
	copy.URL.RawPath = ""
	files.ServeHTTP(response, copy)
}

func apiRequest(request *http.Request) bool {
	requestPath := request.URL.Path
	return requestPath == "/api" || strings.HasPrefix(requestPath, "/api/") ||
		requestPath == "/agent" || strings.HasPrefix(requestPath, "/agent/")
}
