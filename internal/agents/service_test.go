package agents

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"dont/internal/jobs"
	"dont/shared"

	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newAgentTestService(t *testing.T) (*Service, *Store, *jobs.Service, *MemoryTransport) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	db.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "agent_test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobStore := jobs.NewStore(db, "agent_test_")
	if err := jobStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	transport := NewMemoryTransport()
	service, err := NewService(store, jobService, transport)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, jobService, transport
}

func TestAgentSyncOfflineProtectionAndForget(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	items, available, err := service.Agents()
	if err != nil || !available || len(items) != 2 {
		t.Fatalf("agents=%#v available=%v err=%v", items, available, err)
	}
	primary, err := service.Agent("agent-primary")
	if err != nil || primary.Status != StatusOnline || primary.Metrics.LogicalProcessors != 16 || primary.Metrics.PhysicalCores != 8 || primary.Version == "" {
		t.Fatalf("primary=%#v err=%v", primary, err)
	}
	if primary.Capacity.State != CapacityAvailable || primary.Capacity.RecommendedShardLimit != 7 || primary.Capacity.AvailableSlots != 5 {
		t.Fatalf("primary capacity=%#v", primary.Capacity)
	}
	if err := service.Forget(primary.ID); !errors.Is(err, ErrAgentOnline) {
		t.Fatalf("forget online error=%v", err)
	}
	if _, err := service.RunCommand("agent-offline", CommandInput{Action: ActionSystemRefresh, TimeoutSeconds: 30}); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("offline command error=%v", err)
	}
	if err := service.Forget("agent-offline"); err != nil {
		t.Fatal(err)
	}
	items, _, _ = service.Agents()
	if len(items) != 1 || items[0].ID != "agent-primary" {
		t.Fatalf("forgotten offline snapshots must stay removed, got %#v", items)
	}
}

func TestRuntimeTargetsPreferLocalAndKeepRemoteConfigurationIsolated(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	localRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		DisplayName: "本机", SavePath: localRoot, ServerPath: localRoot, LuaBinary: "lua", ServerMode: "64",
	})

	items, err := service.RuntimeTargets()
	if err != nil || len(items) != 3 {
		t.Fatalf("targets=%#v err=%v", items, err)
	}
	if items[0].ID != "local" || !items[0].Default || items[0].Kind != RuntimeKindLocal || items[0].Status != RuntimeStatusReady {
		t.Fatalf("local target=%#v", items[0])
	}
	if items[1].Configured || items[1].Status != RuntimeStatusConfigurationRequired {
		t.Fatalf("unconfigured remote target=%#v", items[1])
	}

	input := RuntimeConfig{
		DisplayName: "生产节点", SavePath: "/srv/dst/save", BackupPath: "/srv/dst/backups",
		ServerPath: "/srv/dst/server", SteamCMDPath: "/usr/games/steamcmd", LuaBinary: "lua5.4", ServerMode: "64",
	}
	target, err := service.SaveRuntimeConfig("agent-primary", input)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "agent:agent-primary" || target.Default || !target.Configured || target.Status != RuntimeStatusReady {
		t.Fatalf("configured target=%#v", target)
	}
	if target.Config.SavePath != input.SavePath || items[0].Config.SavePath != localRoot {
		t.Fatalf("remote config leaked into local target: local=%#v remote=%#v", items[0].Config, target.Config)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{DisplayName: "bad", SavePath: "relative", ServerPath: "/srv/dst"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("relative remote path error=%v", err)
	}
	offline, err := service.SaveRuntimeConfig("agent-offline", input)
	if err != nil || offline.Status != RuntimeStatusOffline {
		t.Fatalf("offline target=%#v err=%v", offline, err)
	}
	if err := service.DeleteRuntimeConfig("agent-primary"); err != nil {
		t.Fatal(err)
	}
	target, err = service.RuntimeTarget("agent-primary")
	if err != nil || target.Configured || target.Status != RuntimeStatusConfigurationRequired {
		t.Fatalf("deleted target=%#v err=%v", target, err)
	}
}

func TestRuntimeConfigBindsOnlyAgentAdvertisedInstallation(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]string{{
		"id": "container", "driver": "container", "save_path": "/srv/dst/saves", "server_path": "/srv/dst/server",
		"steamcmd_path": "/usr/games/steamcmd", "ugc_path": "/srv/dst/workshop", "workshop_content_path": "/srv/dst/workshop/content", "server_mode": "64",
	}}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	agent, err := service.Agent("agent-primary")
	if err != nil || !agent.InstallationRegistrySupported || len(agent.Installations) != 1 || agent.Installations[0].ID != "container" {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	target, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "container", DisplayName: "容器节点", BackupPath: "/srv/dst/backups", LuaBinary: "lua",
	})
	if err != nil || target.Status != RuntimeStatusReady || target.Config.SavePath != "/srv/dst/saves" || target.Config.ServerPath != "/srv/dst/server" {
		t.Fatalf("target=%#v err=%v", target, err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{InstallationID: "missing", DisplayName: "错误节点"}); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("missing installation error=%v", err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "container", DisplayName: "错误路径", SavePath: "/other/saves", ServerPath: "/srv/dst/server", ServerMode: "64",
	}); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("mismatched path error=%v", err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "container", DisplayName: "错误 SteamCMD", SteamCMDPath: "/other/steamcmd",
	}); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("unadvertised optional path error=%v", err)
	}
}

func TestRuntimeConfigRejectsAgentWithSupportedButEmptyInstallationRegistry(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]string{}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	agent, err := service.Agent("agent-primary")
	if err != nil || !agent.InstallationRegistrySupported || len(agent.Installations) != 0 {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "default", DisplayName: "未登记节点", SavePath: "/srv/dst/saves", ServerPath: "/srv/dst/server",
	}); !errors.Is(err, ErrRuntimeInstallationNotRegistered) {
		t.Fatalf("empty registry error=%v", err)
	}
}

func TestExecuteRuntimeBindsTrustedInstallationAndCapability(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	config := RuntimeConfig{
		InstallationID: "primary", DisplayName: "运行节点", SavePath: "/srv/dst/save",
		ServerPath: "/srv/dst/server", ServerMode: "64",
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "operation-runtime-1",
		Action: shared.RuntimeActionConsoleHealth, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
	}
	result, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 30)
	if err != nil || result.Result.InstallationID != "primary" || result.Result.Action != shared.RuntimeActionConsoleHealth {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := service.ExecuteRuntime(context.Background(), "local", request, 30); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("local target error=%v", err)
	}
}

func TestExecuteRuntimeAllowsBoundedGameUpdateTimeout(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	config := RuntimeConfig{InstallationID: "primary", DisplayName: "运行节点", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server", ServerMode: "64"}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "operation-game-update-1",
		Action: shared.RuntimeActionGameVersionUpdate, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		GameVersion: &shared.RuntimeGameVersionRequest{ExpectedVersion: "747465"},
	}
	result, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 1800)
	if err != nil || result.Result.GameVersion == nil || result.Result.GameVersion.CurrentVersion != "747465" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 1801); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized timeout error=%v", err)
	}
	request.Action = shared.RuntimeActionConsoleHealth
	request.GameVersion = nil
	if _, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 301); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ordinary runtime timeout error=%v", err)
	}
}

func TestExecuteRuntimeAllowsCPULifecycleActions(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	config := RuntimeConfig{InstallationID: "primary", DisplayName: "运行节点", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server", ServerMode: "64"}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "operation-cpu-prepare",
		Action: shared.RuntimeActionCPUPrepare, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		CPU: &shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyExclusive, LogicalCPUIds: []int{2, 3}},
	}
	for _, action := range []shared.RuntimeAction{shared.RuntimeActionCPUPrepare, shared.RuntimeActionCPUApply, shared.RuntimeActionCPUObserve} {
		request.OperationID = "operation-" + string(action)
		request.Action = action
		result, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 60)
		if err != nil || result.Result.CPU == nil || result.Result.CPU.Policy != shared.RuntimeCPUPolicyExclusive {
			t.Fatalf("action=%s result=%#v err=%v", action, result, err)
		}
	}
	request.OperationID = "operation-cpu-release"
	request.Action = shared.RuntimeActionCPUApply
	request.CPU = &shared.RuntimeCPURequest{Policy: shared.RuntimeCPUPolicyNone}
	result, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 60)
	if err != nil || result.Result.CPU == nil || result.Result.CPU.State != shared.RuntimeCPUStateReleased {
		t.Fatalf("release result=%#v err=%v", result, err)
	}
}

func TestRuntimeTargetInventoriesCollectsConfiguredLocalTarget(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	localRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		DisplayName: "本机", SavePath: localRoot + string(os.PathSeparator),
		ServerPath: localRoot + string(os.PathSeparator), LuaBinary: "lua", ServerMode: "64",
	})

	items, err := service.RuntimeTargetInventories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || items[0].Target.ID != "local" || !items[0].Available || items[0].Stale {
		t.Fatalf("local inventory=%#v", items)
	}
	if items[0].Inventory.Installation.SavePath != localRoot || items[0].Inventory.Installation.ServerPath != localRoot {
		t.Fatalf("local paths were not normalized: %#v", items[0].Inventory.Installation)
	}
	if items[0].Capacity.PhysicalCores < 1 || items[0].Capacity.ReservedPhysicalCores != 1 {
		t.Fatalf("local capacity=%#v", items[0].Capacity)
	}
}

func TestNormalizeInventoryAcceptsEquivalentWindowsPathSeparators(t *testing.T) {
	now := time.Now().UTC()
	report := shared.RuntimeInventoryReport{
		ProtocolVersion: shared.RuntimeInventoryProtocolVersion,
		ObservedAt:      now,
		CPU:             shared.CPUInventory{LogicalProcessors: 8, PhysicalCores: 4},
		Installation: shared.RuntimeInstallationReport{
			SavePath: `C:\dst\save`, ServerPath: `C:\dst\server`,
		},
	}
	config := RuntimeConfig{SavePath: `C:\dst\save\`, ServerPath: `C:/dst/server/`}
	normalized, err := normalizeInventory(report, config)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Installation.SavePath != `C:\dst\save` || normalized.Installation.ServerPath != `C:\dst\server` {
		t.Fatalf("windows paths=%#v", normalized.Installation)
	}
}

func TestAgentCommandsPersistSuccessAndFailure(t *testing.T) {
	service, _, jobService, _ := newAgentTestService(t)
	job, err := service.RunCommand("agent-primary", CommandInput{Action: ActionSystemRefresh, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitAgentJob(t, jobService, job.ID)
	if completed.Status != jobs.StatusSucceeded {
		t.Fatalf("success job=%#v", completed)
	}
	job, err = service.RunCommand("agent-primary", CommandInput{Action: ActionDiskInspect, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	completed = waitAgentJob(t, jobService, job.ID)
	if completed.Status != jobs.StatusFailed || completed.Targets[0].Error == nil {
		t.Fatalf("failure job=%#v", completed)
	}
	commands, err := service.Commands(CommandFilter{AgentID: "agent-primary", Limit: 25})
	if err != nil || commands.Total != 2 || commands.Items[0].Status != CommandFailed || commands.Items[1].Status != CommandSucceeded {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
	if !strings.Contains(commands.Items[0].Error, "磁盘检查失败") {
		t.Fatalf("failure detail=%#v", commands.Items[0])
	}
	detail, err := service.Command(commands.Items[0].ID)
	if err != nil || detail.ID != commands.Items[0].ID || detail.Status != CommandFailed {
		t.Fatalf("command detail=%#v err=%v", detail, err)
	}
	filtered, err := service.Commands(CommandFilter{Query: "disk", Status: CommandFailed, StartAt: utcAgentTimePointer(time.Now().Add(-time.Hour)), EndAt: utcAgentTimePointer(time.Now().Add(time.Hour)), Limit: 25})
	if err != nil || filtered.Total != 1 || filtered.Items[0].ID != detail.ID {
		t.Fatalf("filtered commands=%#v err=%v", filtered, err)
	}
}

func TestExecuteShardUsesConfiguredInstallationAndTypedTransport(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	_, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "primary", DisplayName: "生产节点", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server", ServerMode: "64",
	})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	request := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "operation-1", OperationKey: "key-1",
		Action: shared.ShardActionStart, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
		LeaseID: "lease-1", FencingToken: 1, LeaseExpiresAt: &expires,
	}
	result, err := service.ExecuteShard(context.Background(), "agent:agent-primary", request, 30)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result.InstallationID != "primary" || result.Result.Action != shared.ShardActionStart || result.Result.Status.State != "running" {
		t.Fatalf("result=%#v", result)
	}
	request.InstallationID = "other"
	if _, err := service.ExecuteShard(context.Background(), "agent:agent-primary", request, 30); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mismatched installation error=%v", err)
	}
}

func TestAgentSecurityMasksAndRotatesOnce(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	before, _ := transport.CurrentKey()
	status, err := service.Security()
	if err != nil || !status.Available || !status.Configured || strings.Contains(status.MaskedKey, before) || len(status.Fingerprint) != 64 {
		t.Fatalf("security=%#v err=%v", status, err)
	}
	if _, err := service.RotateKey(context.Background(), RotateKeyInput{Confirmation: "yes"}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("bad confirmation error=%v", err)
	}
	result, err := service.RotateKey(context.Background(), RotateKeyInput{Confirmation: "ROTATE AGENT KEY"})
	if err != nil || result.NewKey == "" || result.NewKey == before || len(result.Fingerprint) != 64 {
		t.Fatalf("rotation=%#v err=%v", result, err)
	}
	after, err := service.Security()
	if err != nil || after.RotatedAt == nil || after.Fingerprint != result.Fingerprint || strings.Contains(after.MaskedKey, result.NewKey) {
		t.Fatalf("after=%#v err=%v", after, err)
	}
}

func TestAgentInterruptedCommandRecovery(t *testing.T) {
	service, store, jobService, transport := newAgentTestService(t)
	now := time.Now().UTC()
	command := Command{ID: "00000000-0000-0000-0000-000000000001", AgentID: "agent-primary", AgentName: "Node", Action: ActionSystemRefresh, Status: CommandRunning, CreatedAt: now}
	if err := store.CreateCommand(command); err != nil {
		t.Fatal(err)
	}
	if err := store.StartCommand(command.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(store, jobService, transport); err != nil {
		t.Fatal(err)
	}
	commands, err := service.Commands(CommandFilter{Limit: 25})
	if err != nil || len(commands.Items) != 1 || commands.Items[0].Status != CommandFailed || !strings.Contains(commands.Items[0].Error, "服务重启") {
		t.Fatalf("commands=%#v err=%v", commands, err)
	}
}

func TestAgentInventoryRefreshCapacityAndFreshness(t *testing.T) {
	service, _, jobService, transport := newAgentTestService(t)
	config := RuntimeConfig{
		DisplayName: "生产节点", SavePath: "/srv/dst/save", BackupPath: "/srv/dst/backups",
		ServerPath: "/srv/dst/server", SteamCMDPath: "/usr/games/steamcmd", LuaBinary: "lua", ServerMode: "64",
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Inventory("agent-primary"); !errors.Is(err, ErrInventoryNotFound) {
		t.Fatalf("inventory before refresh error=%v", err)
	}
	job, err := service.RefreshInventory("agent-primary")
	if err != nil {
		t.Fatal(err)
	}
	if completed := waitAgentJob(t, jobService, job.ID); completed.Status != jobs.StatusSucceeded {
		t.Fatalf("inventory job=%#v", completed)
	}
	snapshot, err := service.Inventory("agent-primary")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Stale || len(snapshot.Inventory.Rooms) != 1 || len(snapshot.Inventory.Rooms[0].Shards) != 2 || len(snapshot.Inventory.Processes) != 2 {
		t.Fatalf("inventory=%#v", snapshot)
	}
	if snapshot.Capacity.State != CapacityAvailable || snapshot.Capacity.RecommendedShardLimit != 7 || snapshot.Capacity.AvailableSlots != 5 {
		t.Fatalf("capacity=%#v", snapshot.Capacity)
	}
	transport.mu.Lock()
	value := transport.snapshots["agent-primary"]
	value.Metrics.RunningShardCount = 0
	transport.snapshots["agent-primary"] = value
	transport.mu.Unlock()
	agent, err := service.Agent("agent-primary")
	if err != nil || agent.Capacity.RunningShards != 2 || agent.Capacity.AvailableSlots != 5 {
		t.Fatalf("agent inventory capacity=%#v err=%v", agent.Capacity, err)
	}

	service.now = func() time.Time { return snapshot.ReceivedAt.Add(2 * time.Minute) }
	stale, err := service.Inventory("agent-primary")
	if err != nil || !stale.Stale || stale.StaleReason != "report_expired" || stale.Capacity.State != CapacityUnknown {
		t.Fatalf("stale inventory=%#v err=%v", stale, err)
	}

	transport.mu.Lock()
	value = transport.snapshots["agent-primary"]
	value.Status = StatusOffline
	transport.snapshots["agent-primary"] = value
	transport.mu.Unlock()
	if _, err := service.RefreshInventory("agent-primary"); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("offline refresh error=%v", err)
	}
}

func TestCapacityUsesOnePhysicalCorePerShardBudget(t *testing.T) {
	tests := []struct {
		name     string
		logical  int
		physical int
		running  int
		stale    bool
		state    CapacityState
		limit    int
	}{
		{name: "available", logical: 16, physical: 8, running: 6, state: CapacityAvailable, limit: 7},
		{name: "full", logical: 16, physical: 8, running: 7, state: CapacityFull, limit: 7},
		{name: "overcommitted", logical: 16, physical: 8, running: 8, state: CapacityOvercommitted, limit: 7},
		{name: "estimated physical cores", logical: 12, running: 4, state: CapacityAvailable, limit: 5},
		{name: "stale", logical: 16, physical: 8, running: 2, stale: true, state: CapacityUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := capacityFor(test.logical, test.physical, false, test.running, test.stale)
			if value.State != test.state || value.RecommendedShardLimit != test.limit {
				t.Fatalf("capacity=%#v", value)
			}
			if test.physical == 0 && !test.stale && !value.PhysicalCoreEstimated {
				t.Fatalf("estimated capacity=%#v", value)
			}
		})
	}
}

func waitAgentJob(t *testing.T, service *jobs.Service, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := service.Get(id)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCanceled) {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent job %s did not finish", id)
	return jobs.Job{}
}

func utcAgentTimePointer(value time.Time) *time.Time { utc := value.UTC(); return &utc }
