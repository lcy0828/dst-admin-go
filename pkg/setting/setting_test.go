package setting

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindConfigPathHonorsEnvironment(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "runtime.conf")
	if err := os.WriteFile(configPath, []byte("[server]\n"), 0o600); err != nil {
		t.Fatalf("write runtime config: %v", err)
	}
	t.Setenv("DST_ADMIN_CONFIG", configPath)

	got, err := findConfigPath()
	if err != nil {
		t.Fatalf("find config path: %v", err)
	}
	want, err := filepath.Abs(configPath)
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}
	if got != want {
		t.Fatalf("config path = %q, want %q", got, want)
	}
}

func TestFindConfigPathRejectsMissingEnvironmentFile(t *testing.T) {
	t.Setenv("DST_ADMIN_CONFIG", filepath.Join(t.TempDir(), "missing.conf"))

	if _, err := findConfigPath(); err == nil {
		t.Fatal("expected missing configured file to fail")
	}
}
