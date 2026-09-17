package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeCommonCreatesStableSteamCMDEntry(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	configured := filepath.Join(root, "stable", "steamcmd")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(bin, "steamcmd")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "agent.conf")
	if err := os.WriteFile(config, []byte("[runtime.native]\nSTEAMCMD_PATH = "+configured+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("native-common.sh")
	command := exec.Command("sh", "-c", `. "$1"; ensure_configured_steamcmd "$2"`, "test", script, config)
	command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("ensure SteamCMD: %v: %s", err, output)
	}
	target, err := os.Readlink(configured)
	if err != nil || target != real {
		t.Fatalf("stable entry target=%q error=%v output=%s", target, err, output)
	}
}

func TestNativeCommonRejectsMissingConfiguredSteamCMD(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "agent.conf")
	missing := filepath.Join(root, "missing", "steamcmd")
	if err := os.WriteFile(config, []byte("[runtime.native]\nSTEAMCMD_PATH = "+missing+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", `. "$1"; ensure_configured_steamcmd "$2"`, "test", "native-common.sh", config)
	command.Env = append(os.Environ(), "PATH=/usr/bin:/bin")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no executable SteamCMD was found") {
		t.Fatalf("error=%v output=%s", err, output)
	}
}

func TestNativeSystemdUnitsUseManagedHomeOutsideSrv(t *testing.T) {
	tests := []struct {
		name string
		path string
		home string
	}{
		{name: "agent", path: filepath.Join("..", "systemd", "dst-admin-agent.service"), home: "Environment=HOME=/var/lib/dst-admin-agent/home"},
		{name: "local", path: filepath.Join("..", "systemd", "dst-admin-local.service"), home: "Environment=HOME=/var/lib/dst-admin/home"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			unit := string(content)
			if !strings.Contains(unit, test.home) {
				t.Fatalf("unit %s does not set managed HOME %q", test.path, test.home)
			}
			if strings.Contains(unit, "/srv") {
				t.Fatalf("native unit %s still references /srv", test.path)
			}
		})
	}
}

func TestNativeAgentInstallerRendersConfiguredServiceIdentity(t *testing.T) {
	content, err := os.ReadFile("install-native-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	for _, expected := range []string{
		`s/^User=dst$/User=$service_user/`,
		`s/^Group=dst$/Group=$service_group/`,
		`/var/lib/dst-admin-agent/home`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("Agent installer does not render %q", expected)
		}
	}
}
