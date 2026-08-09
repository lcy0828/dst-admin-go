package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dstinstall "dont/internal/dstserver"
)

func TestClassifyRuntimeLogRejectsExpiredToken(t *testing.T) {
	status := classifyRuntimeLog(`[00:00:03]: [200] Account Failed (6): "E_EXPIRED_TOKEN"
[00:00:03]: !!!! Your Server Will Not Start !!!!`, RuntimeStatus{State: RuntimeStarting, SessionExists: true})
	if status.State != RuntimeFailed || status.Code != "TOKEN_EXPIRED" || !status.SessionExists {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogAcceptsLatestReadySignal(t *testing.T) {
	status := classifyRuntimeLog("No auth token could be found\nServer registered via geo DNS in ap-east\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeRunning {
		t.Fatalf("status = %#v", status)
	}
}

func TestClassifyRuntimeLogRejectsFailureAfterAReadySignal(t *testing.T) {
	status := classifyRuntimeLog("E_EXPIRED_TOKEN\nServer registered via geo DNS\nFailed to bind server port\n", RuntimeStatus{State: RuntimeStarting})
	if status.State != RuntimeFailed || status.Code != "PORT_BIND_FAILED" {
		t.Fatalf("status = %#v", status)
	}
}

func TestBuildStartCommandAddsMacSteamEnvironmentWithoutLeakingArguments(t *testing.T) {
	libraryPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(libraryPath, "steamclient.dylib"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DST_ADMIN_STEAM_CLIENT_LIBRARY_PATH", libraryPath)
	command := buildStartCommand(dstinstall.Layout{Kind: dstinstall.LayoutMac}, "/Applications/DST server", []string{"-cluster", "room one"})
	if !strings.Contains(command, "DYLD_LIBRARY_PATH") || !strings.Contains(command, shellArg(libraryPath)) || !strings.Contains(command, "'/Applications/DST server'") || !strings.Contains(command, "'room one'") {
		t.Fatalf("command = %s", command)
	}
}
