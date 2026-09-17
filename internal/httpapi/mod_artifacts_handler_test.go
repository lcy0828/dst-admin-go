package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/internal/modartifact"
	"dont/internal/moddistribution"

	"github.com/gin-gonic/gin"
)

func TestModArtifactHTTPRequiresGrantAndSupportsRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil {
				if entry.IsDir() {
					_ = os.Chmod(path, 0o700)
				} else {
					_ = os.Chmod(path, 0o600)
				}
			}
			return nil
		})
	})
	paths := map[string]string{
		"cache": filepath.Join(root, "cache"), "state": filepath.Join(root, "state"),
		"server": filepath.Join(root, "server"), "saves": filepath.Join(root, "saves"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := moddistribution.New(moddistribution.Config{
		CacheRoot: paths["cache"], StateRoot: paths["state"], NodeID: "controller",
		Installations: []moddistribution.TrustedInstallation{{ID: "default", NodeID: "controller", ServerPath: paths["server"], SavePath: paths["saves"]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "modinfo.lua"), []byte(strings.Repeat("x", 1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.Import(context.Background(), "1392778117", source, moddistribution.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := modartifact.NewService(manager, filepath.Join(root, "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	location, token, err := service.Issue(context.Background(), "agent:node-one", manifest.WorkshopID, manifest.TreeSHA256, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewModArtifactHandler(service).RegisterDownloads(router)

	request := httptest.NewRequest(http.MethodGet, location.DownloadPath, nil)
	request.Header.Set("Range", "bytes=128-")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Header().Get("Accept-Ranges") != "bytes" ||
		response.Header().Get("Content-Range") != "bytes 128-"+stringInt(location.Size-1)+"/"+stringInt(location.Size) ||
		int64(response.Body.Len()) != location.Size-128 || response.Header().Get("X-DST-Mod-Bundle-SHA256") != location.SHA256 {
		t.Fatalf("unexpected Range response: status=%d headers=%v bytes=%d", response.Code, response.Header(), response.Body.Len())
	}

	request = httptest.NewRequest(http.MethodGet, location.DownloadPath, nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusUnauthorized, "MOD_ARTIFACT_TOKEN_REQUIRED")
}

func stringInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
