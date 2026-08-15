package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/internal/shards"
	"dont/shared"
)

type fakeShardRuntime struct {
	mu     sync.Mutex
	status shards.RuntimeStatus
	calls  []string
	fail   error
}

type fakeContainerOwnershipRuntime struct {
	*fakeShardRuntime
	exists bool
}

type fakeConsoleRecoveryRuntime struct {
	*fakeShardRuntime
	recovered []consoleHazard
}

func (f *fakeConsoleRecoveryRuntime) RecoverConsoleHazard(_ context.Context, cluster, shard string) error {
	f.recovered = append(f.recovered, consoleHazard{Cluster: cluster, Shard: shard})
	return nil
}

func (f *fakeContainerOwnershipRuntime) ManagedRuntimeExists(context.Context, string, string) (bool, error) {
	return f.exists, nil
}

func (runtime *fakeShardRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.status, nil
}

func (runtime *fakeShardRuntime) Start(context.Context, string, string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.calls = append(runtime.calls, "start")
	if runtime.fail != nil {
		return runtime.fail
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}
	return nil
}

func (runtime *fakeShardRuntime) Stop(context.Context, string, string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.calls = append(runtime.calls, "stop")
	if runtime.fail != nil {
		return runtime.fail
	}
	runtime.status = shards.RuntimeStatus{State: shards.RuntimeStopped}
	return nil
}

func (runtime *fakeShardRuntime) Send(context.Context, string, string, string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.calls = append(runtime.calls, "save")
	return runtime.fail
}

func newShardOperationAgent(t *testing.T, runtimeControl *fakeShardRuntime) (*Agent, RuntimeInstallation) {
	t.Helper()
	root := t.TempDir()
	serverPath := filepath.Join(root, "server")
	shardPath := filepath.Join(root, "saves", "Cluster_1", "Master")
	for _, path := range []string{serverPath, shardPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(root, "saves", "Cluster_1", "cluster.ini"), filepath.Join(shardPath, "server.ini")} {
		if err := os.WriteFile(path, []byte("[NETWORK]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	installation := RuntimeInstallation{ID: "default", SavePath: filepath.Join(root, "saves"), ServerPath: serverPath, ServerMode: "64"}
	agent, err := NewAgent(&Config{
		ServerURL: "ws://127.0.0.1:8081/agent", AgentID: "test", KeyFile: filepath.Join(root, "agent.conf"),
		OperationStateFile: filepath.Join(root, "operation-state.json"), RuntimeInstallations: []RuntimeInstallation{installation},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.shardRuntime = func(RuntimeInstallation) (shardRuntimeControl, error) { return runtimeControl, nil }
	return agent, installation
}

func shardOperationRequest(action shared.ShardAction, token uint64) shared.ShardOperationRequest {
	expires := time.Now().UTC().Add(5 * time.Minute)
	return shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-1", OperationKey: "key-1",
		InstallationID: "default", Action: action, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "lease-1", FencingToken: token, LeaseExpiresAt: &expires,
	}
}

func TestShardOperationIsIdempotentAcrossAgentRestart(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	request := shardOperationRequest(shared.ShardActionStart, 1)
	result, err := agent.executeShardOperation(string(request.Action), &request, 10)
	if err != nil || result.Idempotent || result.Status.State != string(shards.RuntimeRunning) {
		t.Fatalf("first result=%#v err=%v", result, err)
	}
	result, err = agent.executeShardOperation(string(request.Action), &request, 10)
	if err != nil || !result.Idempotent || strings.Join(runtimeControl.calls, ",") != "start" {
		t.Fatalf("repeat result=%#v calls=%v err=%v", result, runtimeControl.calls, err)
	}

	restarted, err := NewAgent(&Config{
		ServerURL: "ws://127.0.0.1:8081/agent", AgentID: "test", KeyFile: agent.Config.KeyFile,
		OperationStateFile: agent.Config.OperationStateFile, RuntimeInstallations: []RuntimeInstallation{installation},
	})
	if err != nil {
		t.Fatal(err)
	}
	restartedRuntime := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	restarted.shardRuntime = func(RuntimeInstallation) (shardRuntimeControl, error) { return restartedRuntime, nil }
	result, err = restarted.executeShardOperation(string(request.Action), &request, 10)
	if err != nil || !result.Idempotent || len(restartedRuntime.calls) != 0 {
		t.Fatalf("restart result=%#v calls=%v err=%v", result, restartedRuntime.calls, err)
	}
}

func TestShardOperationRejectsStaleFenceAndReusedKey(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	newer := shardOperationRequest(shared.ShardActionStart, 2)
	if _, err := agent.executeShardOperation(string(newer.Action), &newer, 10); err != nil {
		t.Fatal(err)
	}
	stale := shardOperationRequest(shared.ShardActionStop, 1)
	stale.OperationID, stale.OperationKey = "operation-2", "key-2"
	if _, err := agent.executeShardOperation(string(stale.Action), &stale, 10); err == nil || !strings.Contains(err.Error(), "fencing token") {
		t.Fatalf("stale fence error=%v", err)
	}
	reused := newer
	reused.Action = shared.ShardActionStop
	if _, err := agent.executeShardOperation(string(reused.Action), &reused, 10); err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("reused key error=%v", err)
	}
}

func TestShardOperationPreservesFailedResultAndUnknownPendingState(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}, fail: errors.New("start failed")}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	failed := shardOperationRequest(shared.ShardActionStart, 1)
	result, err := agent.executeShardOperation(string(failed.Action), &failed, 10)
	if err == nil || result.Message != "start failed" {
		t.Fatalf("failed result=%#v err=%v", result, err)
	}
	result, err = agent.executeShardOperation(string(failed.Action), &failed, 10)
	if err == nil || !result.Idempotent || len(runtimeControl.calls) != 1 {
		t.Fatalf("cached failure result=%#v calls=%v err=%v", result, runtimeControl.calls, err)
	}

	pending := shardOperationRequest(shared.ShardActionStop, 2)
	pending.OperationID, pending.OperationKey = "operation-2", "key-2"
	if cached, err := agent.shardState.begin(pending, time.Now().UTC()); err != nil || cached != nil {
		t.Fatalf("begin pending cached=%v err=%v", cached, err)
	}
	if _, err := agent.executeShardOperation(string(pending.Action), &pending, 10); !errors.Is(err, errOperationOutcomeUnknown) {
		t.Fatalf("pending error=%v", err)
	}
}

func TestShardOperationRejectsPathEscapeAndExpiredLease(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	expired := shardOperationRequest(shared.ShardActionStart, 1)
	expires := time.Now().UTC().Add(-time.Minute)
	expired.LeaseExpiresAt = &expires
	if _, err := agent.executeShardOperation(string(expired.Action), &expired, 10); err == nil || !strings.Contains(err.Error(), "租约") {
		t.Fatalf("expired lease error=%v", err)
	}

	escapedRoot := t.TempDir()
	escapedShard := filepath.Join(escapedRoot, "Master")
	if err := os.MkdirAll(escapedShard, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(escapedShard, "server.ini"), []byte("[NETWORK]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(installation.SavePath, "Cluster_1", "Master")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapedShard, filepath.Join(installation.SavePath, "Cluster_1", "Master")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	request := shardOperationRequest(shared.ShardActionStatus, 0)
	request.OperationKey, request.LeaseID, request.LeaseExpiresAt = "", "", nil
	if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "房间目录") {
		t.Fatalf("path escape error=%v", err)
	}
}

func TestRuntimeInstallationsLoadNamedSections(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "agent.conf")
	content := "[agent]\nSERVER_URL = ws://127.0.0.1/agent\n[runtime]\nSAVE_PATH = " + filepath.Join(root, "save-a") + "\nSERVER_PATH = " + filepath.Join(root, "server-a") + "\n[runtime.secondary]\nSAVE_PATH = " + filepath.Join(root, "save-b") + "\nSERVER_PATH = " + filepath.Join(root, "server-b") + "\nSERVER_MODE = 32\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := loadRuntimeInstallations(path)
	if err != nil || len(values) != 2 || values[0].ID != "default" || values[1].ID != "secondary" || values[1].ServerMode != "32" {
		t.Fatalf("installations=%#v err=%v", values, err)
	}
}

func TestRuntimeInstallationsNormalizeContainerDriver(t *testing.T) {
	root := t.TempDir()
	values, err := normalizeRuntimeInstallations([]RuntimeInstallation{{
		ID: "container-a", Driver: "container", SavePath: root, ServerPath: root,
	}})
	if err != nil || len(values) != 1 {
		t.Fatalf("installations=%#v err=%v", values, err)
	}
	value := values[0]
	if value.ContainerEngine != "docker" || value.ConsoleSocket != "/run/dst-admin/tmux/tmux.sock" || value.ConsoleSession != "dst" {
		t.Fatalf("container defaults=%#v", value)
	}
	for _, test := range []RuntimeInstallation{
		{ID: "bad-engine", Driver: "container", SavePath: root, ServerPath: root, ContainerEngine: "sh"},
		{ID: "bad-socket", Driver: "container", SavePath: root, ServerPath: root, ConsoleSocket: "relative.sock"},
		{ID: "bad-driver", Driver: "remote", SavePath: root, ServerPath: root},
	} {
		if _, err := normalizeRuntimeInstallations([]RuntimeInstallation{test}); err == nil {
			t.Fatalf("invalid installation accepted: %#v", test)
		}
	}
	if _, err := normalizeRuntimeInstallations([]RuntimeInstallation{
		{ID: "duplicate", SavePath: root, ServerPath: root},
		{ID: "duplicate", SavePath: root, ServerPath: root},
	}); err == nil {
		t.Fatal("duplicate installation accepted")
	}
}

func TestAgentReusesRuntimeControlPerInstallation(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	var creations int
	agent.shardRuntime = func(RuntimeInstallation) (shardRuntimeControl, error) {
		creations++
		return runtimeControl, nil
	}
	first := shardOperationRequest(shared.ShardActionStatus, 0)
	first.OperationKey, first.LeaseID, first.LeaseExpiresAt = "", "", nil
	second := first
	second.OperationID = "operation-2"
	for _, request := range []shared.ShardOperationRequest{first, second} {
		if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err != nil {
			t.Fatal(err)
		}
	}
	if creations != 1 {
		t.Fatalf("runtime creations=%d", creations)
	}
}

func TestContainerRuntimeRefusesTakeoverAfterStateVolumeLoss(t *testing.T) {
	base := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, base)
	installation.Driver = "container"
	installation.ContainerEngine = "docker"
	installation.ConsoleSocket = "/run/dst-admin/tmux/tmux.sock"
	installation.ConsoleSession = "dst"
	agent.Config.RuntimeInstallations = []RuntimeInstallation{installation}
	agent.shardState = &shardOperationState{path: filepath.Join(t.TempDir(), "lost-state.json"), fresh: true, Version: 1, Rooms: make(map[string]shardRoomState)}
	control := &fakeContainerOwnershipRuntime{fakeShardRuntime: base, exists: true}
	agent.shardRuntimes = map[string]shardRuntimeControl{installation.ID: control}
	request := shardOperationRequest(shared.ShardActionStart, 1)
	if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err == nil || !strings.Contains(err.Error(), "CONTAINER_OWNERSHIP_STATE_LOST") {
		t.Fatalf("takeover error=%v", err)
	}
	t.Setenv("DST_ADMIN_ADOPT_EXISTING_CONTAINERS", "true")
	if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err != nil {
		t.Fatalf("explicit adoption failed: %v", err)
	}
	if !agent.shardState.ownershipInitialized() {
		t.Fatal("ownership sentinel was not persisted")
	}
}

func TestAgentRecoversPersistedIncompleteConsoleOperationAsHazard(t *testing.T) {
	state := &shardOperationState{Version: 1, Rooms: map[string]shardRoomState{
		"native\x00cluster_1": {RuntimeOperations: map[string]rememberedRuntimeOperation{
			"pending":  {Action: shared.RuntimeActionConsoleSend, Cluster: "Cluster_1", Shard: "Master", Completed: false},
			"complete": {Action: shared.RuntimeActionConsoleSend, Cluster: "Cluster_1", Shard: "Caves", Completed: true},
		}},
	}}
	hazards := state.pendingConsoleHazards()
	if len(hazards["native"]) != 1 || hazards["native"][0].Shard != "Master" {
		t.Fatalf("hazards=%#v", hazards)
	}
	control := &fakeConsoleRecoveryRuntime{fakeShardRuntime: &fakeShardRuntime{}}
	agent := &Agent{
		Config: &Config{}, shardRuntime: func(RuntimeInstallation) (shardRuntimeControl, error) { return control, nil },
		shardRuntimes: make(map[string]shardRuntimeControl), consoleHazards: hazards,
	}
	if _, err := agent.runtimeControl(RuntimeInstallation{ID: "native", Driver: "native"}); err != nil {
		t.Fatal(err)
	}
	if len(control.recovered) != 1 || control.recovered[0].Cluster != "Cluster_1" || len(agent.consoleHazards) != 0 {
		t.Fatalf("recovered=%#v remaining=%#v", control.recovered, agent.consoleHazards)
	}
}
