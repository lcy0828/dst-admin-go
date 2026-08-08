package shared

import (
	"bytes"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-ini/ini"
)

func testSecurityKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func assertPrivateMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != PrivateFileMode {
		t.Fatalf("mode = %o, want %o", got, PrivateFileMode)
	}
}

func TestWritePrivateFileReplacesContentAndRestrictsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.conf")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertPrivateMode(t, path)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
}

func TestConfigKeyManagerPreservesConfigRestrictsModeAndDoesNotLogKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	oldKey := testSecurityKey('a')
	newKey := testSecurityKey('b')
	content := "[server]\nSECURITY_KEY = " + oldKey + "\n[paths]\nDST_SAVE_PATH = /srv/dst\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	manager, err := NewKeyManagerWithConfig(path, "server", "SECURITY_KEY")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.StopWatching)
	if err := manager.SetKey(newKey); err != nil {
		t.Fatal(err)
	}
	manager.StopWatching()
	manager.StopWatching()

	assertPrivateMode(t, path)
	config, err := ini.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Section("server").Key("SECURITY_KEY").String(); got != newKey {
		t.Fatalf("saved key mismatch")
	}
	if got := config.Section("paths").Key("DST_SAVE_PATH").String(); got != "/srv/dst" {
		t.Fatalf("unrelated config was not preserved: %q", got)
	}
	if strings.Contains(logs.String(), oldKey) || strings.Contains(logs.String(), newKey) {
		t.Fatalf("logs leaked a communication key: %s", logs.String())
	}
}

func TestValidateSecurityKeyRequiresStrongBase64Value(t *testing.T) {
	for _, value := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if err := ValidateSecurityKey(value); err == nil {
			t.Fatalf("ValidateSecurityKey(%q) succeeded", value)
		}
	}
	if err := ValidateSecurityKey(testSecurityKey('x')); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
}
