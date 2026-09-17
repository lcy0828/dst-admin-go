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

	"dont/internal/operationprogress"
	"dont/internal/shards"
	"dont/shared"
)

type fakeShardRuntime struct {
	mu     sync.Mutex
	status shards.RuntimeStatus
	calls  []string
	fail   error
}

type startupProgressRuntime struct {
	*fakeShardRuntime
	reads int
}

func (r *startupProgressRuntime) Status(context.Context, string, string) (shards.RuntimeStatus, error) {
	r.reads++
	if r.reads == 3 {
		return shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}, nil
	}
	return shards.RuntimeStatus{State: shards.RuntimeStarting, StartupStage: "loading_world", SessionExists: true}, nil
}

func TestAgentStartupProgressReusesReadinessReads(t *testing.T) {
	runtime := &startupProgressRuntime{fakeShardRuntime: &fakeShardRuntime{}}
	var updates []operationprogress.Update
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = operationprogress.WithReporter(ctx, func(update operationprogress.Update) { updates = append(updates, update) })
	status, err := waitForShardState(ctx, runtime, "room", "Master", true)
	if err != nil || status.State != shards.RuntimeRunning || runtime.reads != 3 {
		t.Fatalf("status=%+v reads=%d err=%v", status, runtime.reads, err)
	}
	if len(updates) != 1 || updates[0].Stage != "world.start.loading_world" || updates[0].Percent != 80 {
		t.Fatalf("stage updates=%+v", updates)
	}
}

type fakeContainerOwnershipRuntime struct {
	*fakeShardRuntime
	exists bool
}

type fakeConsoleRecoveryRuntime struct {
	*fakeShardRuntime
	recovered []consoleHazard
}

type stagedAgentStartRuntime struct {
	mu      sync.Mutex
	states  map[string]shards.RuntimeStatus
	started chan string
}

func (runtime *stagedAgentStartRuntime) Status(_ context.Context, _, shard string) (shards.RuntimeStatus, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.states[shard], nil
}

func (runtime *stagedAgentStartRuntime) Start(_ context.Context, _, shard string) error {
	runtime.mu.Lock()
	runtime.states[shard] = shards.RuntimeStatus{State: shards.RuntimeStarting, SessionExists: true}
	runtime.mu.Unlock()
	runtime.started <- shard
	return nil
}

func (runtime *stagedAgentStartRuntime) Stop(_ context.Context, _, shard string) error {
	runtime.mu.Lock()
	runtime.states[shard] = shards.RuntimeStatus{State: shards.RuntimeStopped}
	runtime.mu.Unlock()
	return nil
}

func (runtime *stagedAgentStartRuntime) Send(context.Context, string, string, string) error {
	return nil
}

func (runtime *stagedAgentStartRuntime) markRunning(shardsToMark ...string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for _, shard := range shardsToMark {
		runtime.states[shard] = shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}
	}
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

func newShardOperationAgent(t *testing.T, runtimeControl shardRuntimeControl) (*Agent, RuntimeInstallation) {
	t.Helper()
	root := t.TempDir()
	serverPath := filepath.Join(root, "server")
	masterPath := filepath.Join(root, "saves", "Cluster_1", "Master")
	cavesPath := filepath.Join(root, "saves", "Cluster_1", "Caves")
	for _, path := range []string{serverPath, masterPath, cavesPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join(root, "saves", "Cluster_1", "cluster.ini"),
		filepath.Join(masterPath, "server.ini"),
		filepath.Join(cavesPath, "server.ini"),
	} {
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

func TestShardStartPreservesCurrentDiskRuntimeAssets(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeStopped}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	customCommandsPath := filepath.Join(installation.SavePath, "Cluster_1", "Master", "customcommands.lua")
	customCommands := []byte("-- user managed\nreturn {}\n")
	if err := os.WriteFile(customCommandsPath, customCommands, 0o600); err != nil {
		t.Fatal(err)
	}
	request := shardOperationRequest(shared.ShardActionStart, 1)
	if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(customCommandsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(customCommands) {
		t.Fatalf("customcommands.lua was modified: %q", after)
	}
	runtimeRoot := filepath.Join(installation.SavePath, "Cluster_1", "Master", "dst-admin")
	if _, err := os.Stat(runtimeRoot); !os.IsNotExist(err) {
		t.Fatalf("start published Runtime assets: stat error=%v", err)
	}
}

func TestShardStartDoesNotPublishRuntimeIntoRunningWorld(t *testing.T) {
	runtimeControl := &fakeShardRuntime{status: shards.RuntimeStatus{State: shards.RuntimeRunning, SessionExists: true}}
	agent, installation := newShardOperationAgent(t, runtimeControl)
	request := shardOperationRequest(shared.ShardActionStart, 1)
	if _, err := agent.executeShardOperation(string(request.Action), &request, 10); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(installation.SavePath, "Cluster_1", "Master", "dst-admin")
	if _, err := os.Stat(runtimeRoot); !os.IsNotExist(err) {
		t.Fatalf("running world was modified: stat error=%v", err)
	}
	if len(runtimeControl.calls) != 0 {
		t.Fatalf("running world was restarted: calls=%v", runtimeControl.calls)
	}
}

func TestSiblingShardStartsLaunchBeforeEitherShardIsReady(t *testing.T) {
	runtimeControl := &stagedAgentStartRuntime{
		states: map[string]shards.RuntimeStatus{
			"Master": {State: shards.RuntimeStopped},
			"Caves":  {State: shards.RuntimeStopped},
		},
		started: make(chan string, 2),
	}
	agent, _ := newShardOperationAgent(t, runtimeControl)
	master := shardOperationRequest(shared.ShardActionStart, 1)
	master.OperationID, master.OperationKey = "operation-master", "key-master"
	caves := master
	caves.OperationID, caves.OperationKey, caves.Shard = "operation-caves", "key-caves", "Caves"

	type outcome struct {
		result shared.ShardOperationResult
		err    error
	}
	masterDone := make(chan outcome, 1)
	go func() {
		result, err := agent.executeShardOperation(string(master.Action), &master, 10)
		masterDone <- outcome{result: result, err: err}
	}()
	select {
	case shard := <-runtimeControl.started:
		if shard != "Master" {
			t.Fatalf("first launched shard = %s", shard)
		}
	case <-time.After(time.Second):
		t.Fatal("Master start was not dispatched")
	}

	cavesDone := make(chan outcome, 1)
	go func() {
		result, err := agent.executeShardOperation(string(caves.Action), &caves, 10)
		cavesDone <- outcome{result: result, err: err}
	}()
	select {
	case shard := <-runtimeControl.started:
		if shard != "Caves" {
			t.Fatalf("second launched shard = %s", shard)
		}
	case <-time.After(time.Second):
		t.Fatal("Caves waited for Master readiness")
	}

	runtimeControl.markRunning("Master", "Caves")
	for name, completed := range map[string]<-chan outcome{"Master": masterDone, "Caves": cavesDone} {
		select {
		case value := <-completed:
			if value.err != nil || value.result.Status.State != string(shards.RuntimeRunning) {
				t.Fatalf("%s result=%#v err=%v", name, value.result, value.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s readiness was not observed", name)
		}
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
	if _, err := normalizeRuntimeInstallations([]RuntimeInstallation{
		{ID: "first", SavePath: root, ServerPath: filepath.Join(root, "server-a")},
		{ID: "second", SavePath: root, ServerPath: filepath.Join(root, "server-b")},
	}); err == nil || !strings.Contains(err.Error(), "一个存档只能配置一个 Runtime 所有者") {
		t.Fatalf("duplicate SAVE_PATH error=%v", err)
	}
}

func TestRuntimeInstallationsDeriveWorkshopContentPathFromUGCPath(t *testing.T) {
	root := t.TempDir()
	ugcPath := filepath.Join(root, "steamapps", "workshop")
	values, err := normalizeRuntimeInstallations([]RuntimeInstallation{{
		ID: "default", SavePath: root, ServerPath: root, UGCPath: ugcPath,
	}})
	if err != nil || len(values) != 1 {
		t.Fatalf("installations=%#v err=%v", values, err)
	}
	expected := filepath.Join(ugcPath, "content", "322330")
	if values[0].WorkshopContentPath != expected {
		t.Fatalf("workshop content path=%q expected=%q", values[0].WorkshopContentPath, expected)
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
