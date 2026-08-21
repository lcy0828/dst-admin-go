package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

type recordingGameVersionRunner struct {
	version    string
	executable string
	arguments  []string
}

func (r *recordingGameVersionRunner) Run(ctx context.Context, executable string, arguments []string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.executable = executable
	r.arguments = append([]string(nil), arguments...)
	root := arguments[1]
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte(r.version+"\n"), 0o600); err != nil {
		return err
	}
	_, err := io.WriteString(output, "updated\n")
	return err
}

func gameVersionRequest(action shared.RuntimeAction, expected string) shared.RuntimeOperationRequest {
	request := runtimeOperationRequest(action)
	request.GameVersion = &shared.RuntimeGameVersionRequest{ExpectedVersion: expected}
	if action == shared.RuntimeActionGameVersionObserve {
		request.OperationKey, request.LeaseID, request.FencingToken, request.LeaseExpiresAt = "", "", 0, nil
	}
	return request
}

func TestGameVersionUpdateUsesTrustedFixedCommandAndVerifiesVersion(t *testing.T) {
	runtimeControl := &fakeShardRuntime{}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	steamcmd := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(steamcmd, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installation.SteamCMDPath = steamcmd
	agent.Config.RuntimeInstallations[0] = installation
	runner := &recordingGameVersionRunner{version: "747465"}
	agent.gameVersionRunner = runner

	request := gameVersionRequest(shared.RuntimeActionGameVersionUpdate, "747465")
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 1800)
	if err != nil || result.GameVersion == nil || result.GameVersion.CurrentVersion != "747465" || result.Outcome != shared.RuntimeOutcomeConfirmed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	want := []string{"+force_install_dir", installation.ServerPath, "+login", "anonymous", "+app_update", "343050", "validate", "+quit"}
	if runner.executable != steamcmd || !reflect.DeepEqual(runner.arguments, want) {
		t.Fatalf("executable=%q arguments=%v", runner.executable, runner.arguments)
	}
}

func TestGameVersionUpdateFailsWhenInstalledVersionDoesNotMatch(t *testing.T) {
	agent, installation := newShardOperationAgent(t, &fakeShardRuntime{})
	steamcmd := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.WriteFile(steamcmd, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installation.SteamCMDPath = steamcmd
	agent.Config.RuntimeInstallations[0] = installation
	agent.gameVersionRunner = &recordingGameVersionRunner{version: "747464"}

	request := gameVersionRequest(shared.RuntimeActionGameVersionUpdate, "747465")
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 1800)
	if err == nil || !strings.Contains(err.Error(), "与目标版本") || result.Outcome != shared.RuntimeOutcomeFailed || result.GameVersion == nil || result.GameVersion.CurrentVersion != "747464" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestGameVersionObserveDoesNotRequireExistingShard(t *testing.T) {
	agent, installation := newShardOperationAgent(t, &fakeShardRuntime{})
	if err := os.WriteFile(filepath.Join(installation.ServerPath, "version.txt"), []byte("747465\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := gameVersionRequest(shared.RuntimeActionGameVersionObserve, "")
	request.Cluster, request.Shard = "MissingCluster", "MissingShard"
	result, err := agent.executeRuntimeOperation(string(request.Action), &request, 30)
	if err != nil || result.GameVersion == nil || !result.GameVersion.Installed || result.GameVersion.CurrentVersion != "747465" ||
		result.GameVersion.AppID != "343050" || result.GameVersion.UpdateMethod != "steamcmd" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRuntimeInstallationRejectsRelativeSteamCMDPath(t *testing.T) {
	root := t.TempDir()
	_, err := normalizeRuntimeInstallations([]RuntimeInstallation{
		{ID: "default", SavePath: filepath.Join(root, "save"), ServerPath: filepath.Join(root, "server"), SteamCMDPath: "bin/steamcmd"},
	})
	if err == nil || !strings.Contains(err.Error(), "无效路径") {
		t.Fatalf("error=%v", err)
	}
}

func TestGameVersionPayloadRejectsUnexpectedFields(t *testing.T) {
	request := gameVersionRequest(shared.RuntimeActionGameVersionUpdate, "747465")
	request.Mod = &shared.RuntimeModRequest{}
	if err := validateRuntimeOperationRequest(string(request.Action), request, 1800, agentNow()); err == nil || !strings.Contains(err.Error(), "负载无效") {
		t.Fatalf("error=%v", err)
	}
	request.Mod = nil
	request.GameVersion.ExpectedVersion = "; rm -rf /"
	if err := validateRuntimeOperationRequest(string(request.Action), request, 1800, agentNow()); err == nil || !strings.Contains(err.Error(), "目标游戏版本无效") {
		t.Fatalf("error=%v", err)
	}
}

func agentNow() time.Time { return time.Now().UTC() }
