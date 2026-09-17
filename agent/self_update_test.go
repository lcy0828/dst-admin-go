package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dont/shared"
)

func TestResolveAgentUpgradeURLUsesConfiguredController(t *testing.T) {
	value, err := resolveAgentUpgradeURL("wss://192.0.2.42:8443/agent?key=ignored", "/agent-updates/123")
	if err != nil || value != "https://192.0.2.42:8443/agent-updates/123" {
		t.Fatalf("url=%q err=%v", value, err)
	}
	for _, invalid := range []string{"https://controller/agent", "ws://user:pass@controller/agent"} {
		if _, err := resolveAgentUpgradeURL(invalid, "/agent-updates/123"); err == nil {
			t.Fatalf("accepted invalid controller URL %q", invalid)
		}
	}
	if _, err := resolveAgentUpgradeURL("ws://controller/agent", "https://other/agent-updates/123"); err == nil {
		t.Fatal("accepted cross-host download URL")
	}
}

func TestAgentUpgradeDownloadValidatesTokenSizeAndDigest(t *testing.T) {
	payload := []byte("agent-binary")
	digest := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer short-lived" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = response.Write(payload)
	}))
	defer server.Close()
	path, err := downloadAgentRelease(context.Background(), server.URL, "short-lived", t.TempDir(), int64(len(payload)), hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if _, err := downloadAgentRelease(context.Background(), server.URL, "wrong", t.TempDir(), int64(len(payload)), hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("download accepted invalid bearer token")
	}
}

func TestReplaceAgentExecutableKeepsPreviousVersion(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "dst-admin-agent")
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(current, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceAgentExecutable(current, replacement); err != nil {
		t.Fatal(err)
	}
	newData, _ := os.ReadFile(current)
	oldData, _ := os.ReadFile(current + ".previous")
	if string(newData) != "new" || string(oldData) != "old" {
		t.Fatalf("current=%q previous=%q", newData, oldData)
	}
}

func TestReplaceAgentExecutableRestoresPreviousVersionWhenDirectorySyncFails(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "dst-admin-agent")
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(current, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	syncFailure := errors.New("simulated directory sync failure")
	if err := replaceAgentExecutableWithSync(current, replacement, func(string) error { return syncFailure }); !errors.Is(err, syncFailure) {
		t.Fatalf("replace error=%v", err)
	}
	currentData, readErr := os.ReadFile(current)
	if readErr != nil || string(currentData) != "old" {
		t.Fatalf("current=%q err=%v", currentData, readErr)
	}
	if _, err := os.Stat(current + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left stale backup: %v", err)
	}
}

func TestValidateAgentUpgradeRequestRequiresCurrentPlatform(t *testing.T) {
	digest := strings.Repeat("a", 64)
	request := shared.AgentUpgradeRequest{
		ProtocolVersion: shared.AgentUpgradeProtocolVersion, ReleaseID: "release", Version: "2.10.0",
		OS: runtime.GOOS, Arch: runtime.GOARCH, DownloadPath: "/agent-updates/release",
		DownloadToken: "token", SHA256: digest, Size: 1,
	}
	if err := validateAgentUpgradeRequest(request); err != nil {
		t.Fatal(err)
	}
	request.OS = "other"
	if err := validateAgentUpgradeRequest(request); err == nil {
		t.Fatal("accepted a package for another OS")
	}
}
