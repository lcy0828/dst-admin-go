package agents

import (
	"context"
	"errors"
	"os"
	"runtime"
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
	service, store, _, _ := newAgentTestService(t)
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
	if _, err := service.RenameRuntimeTarget("agent:agent-offline", "退役机器"); err != nil {
		t.Fatal(err)
	}
	if err := service.Forget("agent-offline"); err != nil {
		t.Fatal(err)
	}
	displayNames, err := store.NodeDisplayNames()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := displayNames["agent:agent-offline"]; exists {
		t.Fatalf("forgotten Agent display name must be removed: %#v", displayNames)
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
	if runtime.GOOS != "windows" && (!containsString(items[0].Capabilities, "runtime.game-update.v1") || !containsString(items[0].Capabilities, "shard.control.v1")) {
		t.Fatalf("local product capabilities=%#v", items[0].Capabilities)
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
	if target.Name != "林火节点" || target.Config.DisplayName != "生产节点" {
		t.Fatalf("machine name must remain separate from runtime name: %#v", target)
	}
	if target.Config.SavePath != input.SavePath || items[0].Config.SavePath != localRoot {
		t.Fatalf("remote config leaked into local target: local=%#v remote=%#v", items[0].Config, target.Config)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{DisplayName: "bad", SavePath: "relative", ServerPath: "/srv/dst"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("relative remote path error=%v", err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{DisplayName: "bad", SavePath: "/srv/dst", ServerPath: "/srv/dst", ServerMode: "luajit"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("LuaJIT must not be accepted as a server architecture: %v", err)
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

func TestLocalRuntimeInstallationsRemainOneMachineTarget(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	defaultRoot := t.TempDir()
	secondaryRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		InstallationID: "primary", DisplayName: "主安装", SavePath: defaultRoot, ServerPath: defaultRoot,
	})
	if err := service.ConfigureLocalRuntimeInstallation(RuntimeConfig{
		InstallationID: "testing", DisplayName: "测试安装", SavePath: secondaryRoot, ServerPath: secondaryRoot,
	}, false); err != nil {
		t.Fatal(err)
	}
	targets, err := service.RuntimeTargets()
	if err != nil {
		t.Fatal(err)
	}
	localTargets := 0
	for _, target := range targets {
		if target.ID != "local" {
			continue
		}
		localTargets++
		if target.Config.InstallationID != "primary" || len(target.Installations) != 2 ||
			target.Installations[0].ID != "primary" || target.Installations[1].ID != "testing" {
			t.Fatalf("local target=%#v", target)
		}
	}
	if localTargets != 1 {
		t.Fatalf("local targets=%d", localTargets)
	}
	if err := service.ConfigureLocalRuntimeInstallation(RuntimeConfig{InstallationID: "bad id"}, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid installation error=%v", err)
	}
}

func TestRuntimeTargetMachineNamesPersistIndependentlyFromRuntimeConfiguration(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	localRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		DisplayName: "本机运行环境", SavePath: localRoot, ServerPath: localRoot, LuaBinary: "lua", ServerMode: "64",
	})

	local, err := service.RenameRuntimeTarget("local", "书房主机")
	if err != nil || local.Name != "书房主机" || local.Hostname == "" {
		t.Fatalf("renamed local target=%#v err=%v", local, err)
	}
	remote, err := service.RenameRuntimeTarget("agent:agent-primary", "云服务器 A")
	if err != nil || remote.Name != "云服务器 A" || remote.Hostname == "" {
		t.Fatalf("renamed remote target=%#v err=%v", remote, err)
	}
	agent, err := service.Agent("agent-primary")
	if err != nil || agent.DisplayName != "云服务器 A" || agent.Hostname != "林火节点" {
		t.Fatalf("agent machine name=%#v err=%v", agent, err)
	}

	config := RuntimeConfig{
		DisplayName: "远程 DST 安装", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server", ServerMode: "64",
	}
	remote, err = service.SaveRuntimeConfig("agent-primary", config)
	if err != nil || remote.Name != "云服务器 A" || remote.Config.DisplayName != "远程 DST 安装" {
		t.Fatalf("machine alias must remain independent from runtime config: target=%#v err=%v", remote, err)
	}

	items, err := service.RuntimeTargets()
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Name != "书房主机" || items[1].Name != "云服务器 A" {
		t.Fatalf("persisted machine names missing: %#v", items)
	}
	if items[1].Hostname == items[1].Name {
		t.Fatalf("hostname must remain a separate technical identifier: %#v", items[1])
	}

	for _, invalid := range []struct {
		targetID string
		name     string
	}{
		{"local", ""},
		{"local", "bad\nname"},
		{"missing", "有效名称"},
		{"agent:missing", "有效名称"},
	} {
		_, err := service.RenameRuntimeTarget(invalid.targetID, invalid.name)
		if invalid.targetID == "local" {
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("rename %q to %q error=%v", invalid.targetID, invalid.name, err)
			}
		} else if !errors.Is(err, ErrRuntimeTargetNotFound) {
			t.Fatalf("rename missing target %q error=%v", invalid.targetID, err)
		}
	}
}

func TestRuntimeTargetsCanRunWithoutControllerLocalInstallation(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	service.DisableLocalRuntime()
	items, err := service.RuntimeTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != RuntimeKindAgent {
		t.Fatalf("controller-only targets = %#v", items)
	}
	if targetID := service.DefaultRuntimeTargetID(items); targetID != "" {
		t.Fatalf("default target = %q, want empty without an online configured Agent", targetID)
	}
}

func TestRuntimeTopologyListenerCoversTargetInventoryAndLifecycleChanges(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	notifications := 0
	service.AddRuntimeTopologyListener(func() { notifications++ })

	changed, err := service.Sync()
	if err != nil || !changed || notifications != 1 {
		t.Fatalf("initial sync changed=%v notifications=%d err=%v", changed, notifications, err)
	}
	if changed, err = service.Sync(); err != nil || changed || notifications != 1 {
		t.Fatalf("stable sync changed=%v notifications=%d err=%v", changed, notifications, err)
	}

	target, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "default", DisplayName: "生产节点", SavePath: "/srv/dst/save",
		ServerPath: "/srv/dst/server", LuaBinary: "lua", ServerMode: "64",
	})
	if err != nil || notifications != 2 {
		t.Fatalf("save runtime target=%#v notifications=%d err=%v", target, notifications, err)
	}
	agent, err := service.Agent("agent-primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.observeInventory(context.Background(), agent, target.Config); err != nil || notifications != 3 {
		t.Fatalf("observe inventory notifications=%d err=%v", notifications, err)
	}

	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Status = StatusOffline
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	if changed, err = service.Sync(); err != nil || !changed || notifications != 4 {
		t.Fatalf("offline sync changed=%v notifications=%d err=%v", changed, notifications, err)
	}
	if err := service.DeleteRuntimeConfig("agent-primary"); err != nil || notifications != 5 {
		t.Fatalf("delete runtime notifications=%d err=%v", notifications, err)
	}
	if err := service.Forget("agent-primary"); err != nil || notifications != 6 {
		t.Fatalf("forget notifications=%d err=%v", notifications, err)
	}
}

func TestInventorySignalTracksRuntimeChangesWithoutFollowingMetrics(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	now := time.Now().UTC()
	snapshot := TransportSnapshot{
		ID: "agent-a", Status: StatusOnline, Version: "2.9.0", LastHeartbeat: now,
		Capabilities: []string{"runtime.inventory.read"},
		Metrics:      Metrics{CPUUsage: 10},
		Details: map[string]interface{}{"runtime_installations": []map[string]interface{}{{
			"id": "default", "save_path": "/srv/dst/saves", "server_path": "/srv/dst/server",
		}}},
	}
	if !service.updateInventorySignals([]TransportSnapshot{snapshot}) {
		t.Fatal("initial signal was not detected")
	}
	snapshot.LastHeartbeat = now.Add(time.Minute)
	snapshot.Metrics.CPUUsage = 95
	if service.updateInventorySignals([]TransportSnapshot{snapshot}) {
		t.Fatal("heartbeat or CPU metrics triggered inventory collection")
	}
	snapshot.Details["runtime_installations"] = []map[string]interface{}{{
		"id": "default", "save_path": "/opt/dst/saves", "server_path": "/opt/dst/server",
	}}
	if !service.updateInventorySignals([]TransportSnapshot{snapshot}) {
		t.Fatal("installation change did not trigger inventory collection")
	}
	snapshot.Capabilities = []string{"runtime.inventory.read", "runtime.backup.v1"}
	if !service.updateInventorySignals([]TransportSnapshot{snapshot}) {
		t.Fatal("capability change did not trigger inventory collection")
	}
}

func TestInventoryWakeIsCoalesced(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	service.wakeInventoryRefresh()
	service.wakeInventoryRefresh()
	if len(service.inventoryWake) != 1 {
		t.Fatalf("wake count=%d", len(service.inventoryWake))
	}
}

func TestControllerAgentScenarioReconnectWakesInventoryAndRefreshesCapabilities(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	if _, err := service.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "default", DisplayName: "远程节点", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server",
	}); err != nil {
		t.Fatal(err)
	}
	for len(service.inventoryWake) > 0 {
		<-service.inventoryWake
	}

	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Status = StatusOffline
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	if changed, err := service.Sync(); err != nil || !changed {
		t.Fatalf("offline sync changed=%v err=%v", changed, err)
	}
	offline, err := service.RuntimeTarget("agent-primary")
	if err != nil || offline.Online || offline.Status != RuntimeStatusOffline {
		t.Fatalf("offline target=%#v err=%v", offline, err)
	}

	transport.mu.Lock()
	snapshot = transport.snapshots["agent-primary"]
	snapshot.Status = StatusOnline
	snapshot.Capabilities = []string{"system.report", "runtime.inventory.read", "shard.control.v1"}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	if changed, err := service.Sync(); err != nil || !changed {
		t.Fatalf("reconnect sync changed=%v err=%v", changed, err)
	}
	reconnected, err := service.RuntimeTarget("agent-primary")
	if err != nil || !reconnected.Online || containsString(reconnected.Capabilities, "runtime.backup.v1") {
		t.Fatalf("reconnected target retained stale capabilities: %#v err=%v", reconnected, err)
	}
	if len(service.inventoryWake) != 1 {
		t.Fatalf("reconnect inventory wake count=%d", len(service.inventoryWake))
	}
}

func TestUniqueAdvertisedRuntimeIsDiscoveredAndCollected(t *testing.T) {
	service, store, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]interface{}{{
		"id": "default", "driver": "native", "save_path": "/srv/dst/saves", "server_path": "/srv/dst/server",
		"steamcmd_path": "/usr/games/steamcmd", "ugc_path": "/srv/dst/workshop",
		"workshop_content_path": "/srv/dst/workshop/content/322330", "server_mode": "64",
	}}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	targets, err := service.RuntimeTargets()
	if err != nil {
		t.Fatal(err)
	}
	var discovered RuntimeTarget
	for _, target := range targets {
		if target.AgentID == "agent-primary" {
			discovered = target
			break
		}
	}
	if !discovered.Configured || discovered.Status != RuntimeStatusReady || discovered.Config.Source != RuntimeConfigSourceDiscovered ||
		discovered.Config.InstallationID != "default" || discovered.Config.SavePath != "/srv/dst/saves" {
		t.Fatalf("discovered target=%#v", discovered)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		inventory, inventoryErr := store.Inventory("agent-primary")
		if inventoryErr == nil {
			if inventory.Inventory.Installation.ID != "default" {
				t.Fatalf("inventory=%#v", inventory)
			}
			break
		}
		if !errors.Is(inventoryErr, ErrInventoryNotFound) || time.Now().After(deadline) {
			t.Fatalf("automatic inventory error=%v", inventoryErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	transport.mu.Lock()
	snapshot = transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]interface{}{{
		"id": "default", "driver": "native", "save_path": "/opt/dst/saves", "server_path": "/opt/dst/server",
		"steamcmd_path": "/usr/games/steamcmd", "ugc_path": "/opt/dst/workshop",
		"workshop_content_path": "/opt/dst/workshop/content/322330", "server_mode": "64",
	}}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	target, err := service.RuntimeTarget("agent-primary")
	if err != nil || target.Config.Source != RuntimeConfigSourceDiscovered || target.Config.SavePath != "/opt/dst/saves" ||
		target.Config.ServerPath != "/opt/dst/server" {
		t.Fatalf("updated discovered target=%#v err=%v", target, err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		inventory, inventoryErr := store.Inventory("agent-primary")
		if inventoryErr == nil && inventory.Inventory.Installation.SavePath == "/opt/dst/saves" {
			break
		}
		if inventoryErr != nil && !errors.Is(inventoryErr, ErrInventoryNotFound) {
			t.Fatalf("updated automatic inventory error=%v", inventoryErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("updated automatic inventory was not collected: %#v err=%v", inventory, inventoryErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRuntimeDiscoveryRequiresUniqueInstallationAndHonorsRemoval(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]interface{}{
		{"id": "primary", "driver": "native", "save_path": "/srv/dst/saves-a", "server_path": "/srv/dst/server-a", "server_mode": "64"},
		{"id": "secondary", "driver": "native", "save_path": "/srv/dst/saves-b", "server_path": "/srv/dst/server-b", "server_mode": "64"},
	}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	target, err := service.RuntimeTarget("agent-primary")
	if err != nil || target.Configured {
		t.Fatalf("multiple-installation target=%#v err=%v", target, err)
	}

	transport.mu.Lock()
	snapshot = transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]interface{}{{
		"id": "primary", "driver": "native", "save_path": "/srv/dst/saves-a", "server_path": "/srv/dst/server-a", "server_mode": "64",
	}}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	target, err = service.RuntimeTarget("agent-primary")
	if err != nil || !target.Configured || target.Config.Source != RuntimeConfigSourceDiscovered {
		t.Fatalf("unique-installation target=%#v err=%v", target, err)
	}

	if err := service.DeleteRuntimeConfig("agent-primary"); err != nil {
		t.Fatal(err)
	}
	target, err = service.RuntimeTarget("agent-primary")
	if err != nil || target.Configured {
		t.Fatalf("removed target was automatically recreated: %#v err=%v", target, err)
	}
	agent, err := service.Agent("agent-primary")
	if err != nil || !agent.RuntimeAutoAdoptDisabled {
		t.Fatalf("automatic discovery suppression missing: %#v err=%v", agent, err)
	}

	target, err = service.SaveRuntimeConfig("agent-primary", RuntimeConfig{InstallationID: "primary", DisplayName: "人工运行环境"})
	if err != nil || !target.Configured || target.Config.Source != RuntimeConfigSourceManual {
		t.Fatalf("manual target=%#v err=%v", target, err)
	}
	agent, err = service.Agent("agent-primary")
	if err != nil || agent.RuntimeAutoAdoptDisabled {
		t.Fatalf("manual save did not restore discovery policy: %#v err=%v", agent, err)
	}
}

func TestRuntimeConfigBindsOnlyAgentAdvertisedInstallation(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Details["runtime_installations"] = []map[string]interface{}{{
		"id": "container", "driver": "container", "save_path": "/srv/dst/saves", "server_path": "/srv/dst/server",
		"steamcmd_path": "/usr/games/steamcmd", "ugc_path": "/srv/dst/workshop", "workshop_content_path": "/srv/dst/workshop/content", "server_mode": "64",
		"performance": shared.RuntimePerformanceReport{
			Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceIncompatible,
			GameVersion: "747465", SignatureVersion: "728321",
			SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame}, Issues: []string{"signature_version_mismatch"},
		},
	}}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()

	agent, err := service.Agent("agent-primary")
	if err != nil || !agent.InstallationRegistrySupported || len(agent.Installations) != 1 || agent.Installations[0].ID != "container" || agent.Installations[0].Performance == nil || agent.Installations[0].Performance.Status != shared.RuntimePerformanceIncompatible {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	target, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "container", DisplayName: "容器节点", BackupPath: "/srv/dst/backups", LuaBinary: "lua",
	})
	if err != nil || target.Status != RuntimeStatusReady || target.Config.SavePath != "/srv/dst/saves" || target.Config.ServerPath != "/srv/dst/server" || target.Performance == nil || target.Performance.SignatureVersion != "728321" {
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

func TestNormalizeRuntimePerformanceRejectsUntrustedReadyReports(t *testing.T) {
	valid := shared.RuntimePerformanceReport{
		Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceReady, CanEnable: true,
		PackageVersion: "2.9.1", GameVersion: "747465", SignatureVersion: "747465",
		BinarySHA256: strings.Repeat("a", 64),
		SupportedModes: []shared.RuntimePerformanceMode{
			shared.RuntimePerformanceModeGame,
			shared.RuntimePerformanceModeJITOff,
			shared.RuntimePerformanceModeJITOn,
		},
		Issues: []string{},
	}
	if normalized := normalizeRuntimePerformance(&valid); normalized == nil || !normalized.CanEnable {
		t.Fatalf("valid ready report was rejected: %#v", normalized)
	}
	v3 := valid
	v3.PackageVersion = "3.0.0"
	v3.SignatureVersion = ""
	v3.AutomaticSignatures = true
	if normalized := normalizeRuntimePerformance(&v3); normalized == nil || !normalized.CanEnable || !normalized.AutomaticSignatures {
		t.Fatalf("automatic-signature report was rejected: %#v", normalized)
	}

	tests := []struct {
		name   string
		mutate func(*shared.RuntimePerformanceReport)
	}{
		{name: "agent did not authorize enable", mutate: func(report *shared.RuntimePerformanceReport) { report.CanEnable = false }},
		{name: "signature version mismatch", mutate: func(report *shared.RuntimePerformanceReport) { report.SignatureVersion = "728321" }},
		{name: "required mode missing", mutate: func(report *shared.RuntimePerformanceReport) {
			report.SupportedModes = []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame, shared.RuntimePerformanceModeJITOn}
		}},
		{name: "unknown mode", mutate: func(report *shared.RuntimePerformanceReport) {
			report.SupportedModes = append(report.SupportedModes, shared.RuntimePerformanceMode("future-mode"))
		}},
		{name: "unknown issue", mutate: func(report *shared.RuntimePerformanceReport) { report.Issues = []string{"future_untrusted_issue"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := valid
			report.SupportedModes = append([]shared.RuntimePerformanceMode(nil), valid.SupportedModes...)
			report.Issues = append([]string(nil), valid.Issues...)
			test.mutate(&report)
			if normalized := normalizeRuntimePerformance(&report); normalized != nil {
				t.Fatalf("untrusted report must be rejected: %#v", normalized)
			}
		})
	}
}

func TestNormalizeRuntimePerformanceKeepsNonReadyReportsDisabled(t *testing.T) {
	report := shared.RuntimePerformanceReport{
		Provider: "dontstarve-luajit2", Status: shared.RuntimePerformanceIncompatible, CanEnable: true,
		GameVersion: "747465", SignatureVersion: "728321",
		SupportedModes: []shared.RuntimePerformanceMode{shared.RuntimePerformanceModeGame},
		Issues:         []string{"signature_version_mismatch"},
	}
	normalized := normalizeRuntimePerformance(&report)
	if normalized == nil || normalized.CanEnable || len(normalized.Issues) != 1 {
		t.Fatalf("non-ready report=%#v", normalized)
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

func TestExecuteRuntimeRejectsLegacyRuntimeCapability(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	config := RuntimeConfig{
		InstallationID: "primary", DisplayName: "运行节点", SavePath: "/srv/dst/save",
		ServerPath: "/srv/dst/server", ServerMode: "64",
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	for index, capability := range snapshot.Capabilities {
		snapshot.Capabilities[index] = strings.ReplaceAll(strings.ReplaceAll(capability, "runtime.driver.v2", "runtime.driver.v1"), "runtime.console.v2", "runtime.console.v1")
	}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	request := shared.RuntimeOperationRequest{
		ProtocolVersion: shared.RuntimeOperationProtocolVersion, OperationID: "operation-runtime-legacy",
		Action: shared.RuntimeActionConsoleHealth, Cluster: "Cluster_1", Shard: "Master", TopologyRevision: "revision-1",
	}
	if _, err := service.ExecuteRuntime(context.Background(), "agent:agent-primary", request, 30); !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("legacy runtime capability error=%v", err)
	}
}

func TestDetectEgressRunsOnRemoteRuntimeTarget(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	config := RuntimeConfig{
		InstallationID: "primary", DisplayName: "运行节点", SavePath: "/srv/dst/save",
		ServerPath: "/srv/dst/server", ServerMode: "64",
	}
	if _, err := service.SaveRuntimeConfig("agent-primary", config); err != nil {
		t.Fatal(err)
	}
	result, err := service.DetectEgress(context.Background(), "agent:agent-primary", shared.RuntimeNetworkRegionCN)
	if err != nil || result.Address != "203.0.113.42" || result.Region != shared.RuntimeNetworkRegionCN || result.ObservedAt.IsZero() {
		t.Fatalf("result=%#v err=%v", result, err)
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
	// Reservation policy is covered by the capacity table tests; small CI
	// runners legitimately reserve no core so Master and Caves can both fit.
	if items[0].Capacity.PhysicalCores < 1 || items[0].Capacity.RecommendedShardLimit < 1 || items[0].Capacity.AvailableSlots != items[0].Capacity.RecommendedShardLimit {
		t.Fatalf("local capacity=%#v", items[0].Capacity)
	}
}

func TestRuntimeTargetInventoriesReturnsEveryLocalInstallation(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	primaryRoot := t.TempDir()
	testingRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		InstallationID: "primary", DisplayName: "主安装", SavePath: primaryRoot, ServerPath: primaryRoot, ServerMode: "64",
	})
	if err := service.ConfigureLocalRuntimeInstallation(RuntimeConfig{
		InstallationID: "testing", DisplayName: "测试安装", SavePath: testingRoot, ServerPath: testingRoot, ServerMode: "64",
	}, false); err != nil {
		t.Fatal(err)
	}

	items, err := service.RuntimeTargetInventories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	local := make(map[string]RuntimeTargetInventory)
	for _, item := range items {
		if item.Target.ID == "local" {
			local[item.Target.Config.InstallationID] = item
		}
	}
	if len(local) != 2 || !local["primary"].Available || !local["testing"].Available {
		t.Fatalf("local inventories=%#v", local)
	}
	if local["primary"].Target.DefaultInstallationID != "primary" || len(local["testing"].Target.Installations) != 2 {
		t.Fatalf("installation metadata=%#v", local)
	}
}

type localContainerProcessProvider struct {
	processes []shared.ShardProcessReport
	err       error
}

func (p localContainerProcessProvider) ContainerProcesses(context.Context) ([]shared.ShardProcessReport, error) {
	return append([]shared.ShardProcessReport(nil), p.processes...), p.err
}

func TestRuntimeTargetInventoriesUsesEmbeddedContainerProcesses(t *testing.T) {
	service, _, _, _ := newAgentTestService(t)
	localRoot := t.TempDir()
	service.ConfigureLocalRuntime(RuntimeConfig{
		InstallationID: "default", DisplayName: "本机容器", SavePath: localRoot,
		ServerPath: localRoot, LuaBinary: "lua", ServerMode: "64",
	})
	service.ConfigureLocalContainerProcesses(localContainerProcessProvider{processes: []shared.ShardProcessReport{{
		PID: 42, RuntimeKind: "container", InstanceID: "container@2026-08-22T00:00:00Z",
		Cluster: "Cluster_1", Shard: "Master",
	}}})

	items, err := service.RuntimeTargetInventories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var local *RuntimeTargetInventory
	for index := range items {
		if items[index].Target.ID == "local" {
			local = &items[index]
			break
		}
	}
	if local == nil || len(local.Inventory.Processes) != 1 || local.Inventory.Processes[0].RuntimeKind != "container" {
		t.Fatalf("local container inventory=%#v", items)
	}
	if local.Inventory.Installation.ID != "default" || local.Capacity.RunningShards != 1 {
		t.Fatalf("local inventory=%#v", *local)
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

func TestExecuteShardRequiresConfirmedLifecycleCapabilityForMutations(t *testing.T) {
	service, _, _, transport := newAgentTestService(t)
	_, err := service.SaveRuntimeConfig("agent-primary", RuntimeConfig{
		InstallationID: "primary", DisplayName: "生产节点", SavePath: "/srv/dst/save", ServerPath: "/srv/dst/server", ServerMode: "64",
	})
	if err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	snapshot := transport.snapshots["agent-primary"]
	snapshot.Capabilities = []string{"shard.control.v1"}
	transport.snapshots["agent-primary"] = snapshot
	transport.mu.Unlock()
	if _, err := service.Sync(); err != nil {
		t.Fatal(err)
	}
	status := shared.ShardOperationRequest{
		ProtocolVersion: shared.ShardOperationProtocolVersion, OperationID: "status-1",
		Action: shared.ShardActionStatus, Cluster: "Cluster_1", Shard: "Caves",
	}
	if _, err := service.ExecuteShard(context.Background(), "agent:agent-primary", status, 30); err != nil {
		t.Fatalf("read-only status should remain compatible: %v", err)
	}
	status.OperationID = "stop-1"
	status.Action = shared.ShardActionStop
	if _, err := service.ExecuteShard(context.Background(), "agent:agent-primary", status, 30); !errors.Is(err, ErrUnsupportedAction) {
		t.Fatalf("legacy lifecycle mutation error=%v", err)
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
		{name: "two core default room", logical: 2, physical: 2, running: 2, state: CapacityFull, limit: 2},
		{name: "two vcpu one reported physical core", logical: 2, physical: 1, running: 2, state: CapacityFull, limit: 2},
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

func TestRuntimeTargetFromAgentPreservesReportedIPAddresses(t *testing.T) {
	agent := Agent{
		ID: "node-a", Hostname: "node-a", Status: StatusOnline, LastHeartbeat: time.Now().UTC(),
		IPAddresses: []string{"192.168.2.42", "10.0.0.42"},
	}
	target := runtimeTargetFromAgent(agent, RuntimeConfig{}, false)
	if len(target.IPAddresses) != 2 || target.IPAddresses[0] != "192.168.2.42" || target.IPAddresses[1] != "10.0.0.42" {
		t.Fatalf("target IP addresses=%v", target.IPAddresses)
	}
	target.IPAddresses[0] = "changed"
	if agent.IPAddresses[0] != "192.168.2.42" {
		t.Fatal("runtime target aliases the Agent IP address slice")
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
