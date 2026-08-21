package webui

import (
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Wrap serves a built SPA for non-API GET/HEAD requests. An empty root keeps
// the API-only behavior used by development and split deployments.
func Wrap(api http.Handler, root string) (http.Handler, error) {
	if api == nil {
		return nil, errors.New("API handler is required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return api, nil
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
	index := filepath.Join(absolute, "index.html")
	if info, err = os.Stat(index); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("web UI index.html is unavailable")
	}

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if apiRequest(request) || (request.Method != http.MethodGet && request.Method != http.MethodHead) {
			api.ServeHTTP(response, request)
			return
		}

		cleaned := path.Clean("/" + request.URL.Path)
		relative := filepath.FromSlash(strings.TrimPrefix(cleaned, "/"))
		candidate := filepath.Join(absolute, relative)
		if withinRoot(absolute, candidate) {
			if file, statErr := os.Stat(candidate); statErr == nil && file.Mode().IsRegular() {
				if strings.HasPrefix(cleaned, "/assets/") {
					response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFile(response, request, candidate)
				return
			}
		}
		if strings.HasPrefix(cleaned, "/assets/") || filepath.Ext(cleaned) != "" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(response, request, index)
	}), nil
}

func apiRequest(request *http.Request) bool {
	requestPath := request.URL.Path
	return requestPath == "/api" || strings.HasPrefix(requestPath, "/api/") ||
		requestPath == "/agent" || strings.HasPrefix(requestPath, "/agent/")
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
