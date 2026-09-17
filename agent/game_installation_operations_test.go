package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dont/internal/dstserver"
	"dont/shared"
)

func gameInstallationRequest(action shared.RuntimeAction) shared.RuntimeOperationRequest {
	r := runtimeOperationRequest(action)
	r.Cluster = "NoRoom"
	r.Shard = "NoWorld"
	r.GameInstallation = &shared.GameInstallationRequest{}
	return r
}
func TestGameInstallationPayloadRequiresIntentAndLease(t *testing.T) {
	r := gameInstallationRequest(shared.RuntimeActionGameInstallationInstall)
	if err := validateRuntimeOperationRequest(string(r.Action), r, 1740, agentNow()); err != nil {
		t.Fatal(err)
	}
	r.GameInstallation.Path = "/unregistered/download"
	if err := validateRuntimeOperationRequest(string(r.Action), r, 1740, agentNow()); err == nil {
		t.Fatal("download accepted unregistered destination")
	}
	r.GameInstallation.Path = ""
	r.LeaseID = ""
	if err := validateRuntimeOperationRequest(string(r.Action), r, 1740, agentNow()); err == nil {
		t.Fatal("installation without lease")
	}
	r = gameInstallationRequest(shared.RuntimeActionGameInstallationObserve)
	r.LuaJIT = &shared.RuntimeLuaJITRequest{}
	if err := validateRuntimeOperationRequest(string(r.Action), r, 30, agentNow()); err == nil {
		t.Fatal("mixed payload accepted")
	}
}
func writeInstallationGame(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin64"), 0755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	data[16] = 3
	data[18] = 62
	data[20] = 1
	data[52] = 64
	if err := os.WriteFile(filepath.Join(root, "bin64", dstserver.BinaryX64), data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("747465"), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestGameInstallationAdoptRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory alias")
	}
	a, i := newShardOperationAgent(t, &fakeShardRuntime{})
	source := filepath.Join(filepath.Dir(i.ServerPath), "existing")
	writeInstallationGame(t, source)
	r := gameInstallationRequest(shared.RuntimeActionGameInstallationObserve)
	r.GameInstallation.Path = source
	observed, err := a.executeRuntimeOperation(string(r.Action), &r, 30)
	if err != nil || observed.GameInstallation == nil || observed.GameInstallation.Fingerprint == "" {
		t.Fatalf("inspection=%+v err=%v", observed, err)
	}
	r = gameInstallationRequest(shared.RuntimeActionGameInstallationAdopt)
	r.GameInstallation = &shared.GameInstallationRequest{Path: source, Fingerprint: observed.GameInstallation.Fingerprint}
	adopted, err := a.executeRuntimeOperation(string(r.Action), &r, 30)
	if err != nil || adopted.Outcome != shared.RuntimeOutcomeConfirmed {
		t.Fatalf("adopt=%+v err=%v", adopted, err)
	}
	again, err := a.executeRuntimeOperation(string(r.Action), &r, 30)
	if err != nil || !again.Idempotent {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	restarted, err := NewAgent(&Config{ServerURL: a.Config.ServerURL, AgentID: "test", KeyFile: a.Config.KeyFile, OperationStateFile: a.Config.OperationStateFile, RuntimeInstallations: []RuntimeInstallation{i}})
	if err != nil {
		t.Fatal(err)
	}
	r = gameInstallationRequest(shared.RuntimeActionGameInstallationObserve)
	status, err := restarted.executeRuntimeOperation(string(r.Action), &r, 30)
	if err != nil || status.GameInstallation == nil || !status.GameInstallation.Installed {
		t.Fatalf("restart status=%+v err=%v", status, err)
	}
	if b, err := os.ReadFile(filepath.Join(i.SavePath, "Cluster_1", "cluster.ini")); err != nil || string(b) != "[NETWORK]\n" {
		t.Fatal("existing save configuration changed")
	}
}

type gameInstallRunner struct {
	t     *testing.T
	calls int
}

func (f *gameInstallRunner) Run(_ context.Context, _ string, args []string, _ io.Writer) error {
	f.calls++
	if strings.Join(args[2:], " ") != "+login anonymous +app_update 343050 validate +quit" {
		f.t.Fatal(args)
	}
	writeInstallationGame(f.t, args[1])
	return nil
}
func TestGameInstallationFreshInstall(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("Linux amd64 installation")
	}
	a, i := newShardOperationAgent(t, &fakeShardRuntime{})
	i.SteamCMDPath = "/bin/true"
	a.Config.RuntimeInstallations[0] = i
	runner := &gameInstallRunner{t: t}
	a.gameVersionRunner = runner
	r := gameInstallationRequest(shared.RuntimeActionGameInstallationInstall)
	result, err := a.executeRuntimeOperation(string(r.Action), &r, 1740)
	if err != nil || result.GameInstallation == nil || !result.GameInstallation.Installed || runner.calls != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay, err := a.executeRuntimeOperation(string(r.Action), &r, 1740)
	if err != nil || !replay.Idempotent || runner.calls != 1 {
		t.Fatal("installation ran twice", err)
	}
}
