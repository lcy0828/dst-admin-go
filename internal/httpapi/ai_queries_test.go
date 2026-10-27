package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"dont/internal/agents"
	"dont/internal/aicontrol"
	"dont/internal/automation"
	"dont/internal/backups"
	"dont/internal/mods"
	"dont/internal/players"
	"dont/internal/runtimeobservation"
	"dont/internal/runtimeoverview"
	"dont/internal/topology"
	"dont/shared"
)

type aiQueryFixture struct {
	groupEnabled                    bool
	groupErr, policyErr, refreshErr error
	refreshes                       int
	metadataReads                   int
	nextRun                         time.Time
}

func (f *aiQueryFixture) RefreshPresence(context.Context, string) (players.PresenceSnapshot, error) {
	f.refreshes++
	return players.PresenceSnapshot{Online: 1, Fresh: f.refreshErr == nil, StaleOnline: 1}, f.refreshErr
}
func (*aiQueryFixture) List(_ string, filter players.ListFilter) (players.List, error) {
	if !filter.SkipAccessLists {
		return players.List{}, errors.New("unexpected access list lookup")
	}
	return players.List{Online: 1, StaleOnline: 1, Total: 2, Items: []players.Player{{ID: "KU_REAL", Name: "玩家", Online: true, WorldName: "Master", PresenceStatus: players.FreshnessLive}}}, nil
}

type aiBackupQueryFixture struct{ *aiQueryFixture }

func (f aiBackupQueryFixture) Policy(string) (backups.Policy, error) {
	return backups.Policy{Enabled: false}, f.policyErr
}
func (aiBackupQueryFixture) List(string) ([]backups.Backup, error) {
	return []backups.Backup{{Name: "昨天的手动备份", Kind: backups.KindManual}}, nil
}
func (f *aiQueryFixture) RoomFacts(context.Context, string) (mods.ModList, error) {
	return mods.ModList{Items: []mods.ModState{{SteamMod: mods.SteamMod{ID: "123"}, Configured: true, Enabled: true, Installed: false, ConfiguredWorlds: []string{"Master"}, Warnings: []string{"Agent files unavailable"}}, {SteamMod: mods.SteamMod{ID: "456", Name: "关闭的模组"}, Configured: true}}}, nil
}
func (f *aiQueryFixture) CachedMetadata(context.Context, []string) (map[string]mods.SteamMod, error) {
	f.metadataReads++
	return map[string]mods.SteamMod{"123": {ID: "123", Name: "测试模组"}}, nil
}
func (f *aiQueryFixture) Groups(string) ([]automation.Group, error) {
	return []automation.Group{{ID: "g", Enabled: f.groupEnabled}}, f.groupErr
}
func (f *aiQueryFixture) Tasks(string) ([]automation.Task, error) {
	next := time.Now().Add(time.Hour)
	if !f.nextRun.IsZero() {
		next = f.nextRun
	}
	return []automation.Task{{ID: "backup", GroupID: "g", Name: "每日备份", Enabled: true, Action: automation.ActionBackupCreate, Schedule: "0 3 * * *", Timezone: "Asia/Shanghai", NextRunAt: &next, LastStatus: automation.RunFailed, Parameters: map[string]any{"keep": 7}}, {ID: "update", GroupID: "g", Name: "无人更新", Action: automation.ActionGameUpdateEmpty}}, nil
}

func TestAIQueriesConvertScheduleTimeInsteadOfRelabelingUTC(t *testing.T) {
	f := &aiQueryFixture{groupEnabled: true, nextRun: time.Date(2026, 10, 19, 12, 2, 0, 0, time.UTC)}
	b := &AIBackend{queries: AIPlatformQueries{Automation: f}}
	raw, err := b.Invoke(context.Background(), "room_automation", aicontrol.Arguments{RoomID: "room"}, "room")
	if err != nil || !strings.Contains(string(raw), `"nextRunLocal":"2026-10-19T20:02:00+08:00"`) {
		t.Fatal("UTC task time was not converted", string(raw), err)
	}
	f.groupEnabled = false
	raw, err = b.Invoke(context.Background(), "room_automation", aicontrol.Arguments{RoomID: "room"}, "room")
	if err != nil || strings.Contains(string(raw), "nextRunLocal") {
		t.Fatal("disabled group got a next execution time", string(raw), err)
	}
}
func (*aiQueryFixture) Runs(string, automation.RunFilter) (automation.RunList, error) {
	return automation.RunList{}, nil
}

func TestAIQueriesDistinguishBackupEnablementHistoryAndPartialFailure(t *testing.T) {
	f := &aiQueryFixture{}
	b := &AIBackend{queries: AIPlatformQueries{Backups: aiBackupQueryFixture{f}, Automation: f}}
	read := func() map[string]any {
		t.Helper()
		raw, err := b.Invoke(context.Background(), "room_backups", aicontrol.Arguments{RoomID: "r"}, "r")
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	value := read()
	if value["automaticBackupEnabled"] != false {
		t.Fatal("disabled group or old archive mistaken for automatic backup", value)
	}
	task := value["scheduledBackups"].([]any)[0].(map[string]any)
	if task["effectiveEnabled"] != false || task["nextRunAt"] != nil {
		t.Fatal("disabled group got an active next run", task)
	}
	f.groupEnabled = true
	value = read()
	if value["automaticBackupEnabled"] != true {
		t.Fatal("scheduled backup was ignored", value)
	}
	task = value["scheduledBackups"].([]any)[0].(map[string]any)
	if task["lastStatus"] != "failed" || task["timezone"] != "Asia/Shanghai" || task["nextRunAt"] == nil {
		t.Fatal("backup result or schedule lost", task)
	}
	f.groupErr = errors.New("automation database unavailable")
	f.policyErr = errors.New("policy database unavailable")
	value = read()
	if value["automaticBackupEnabled"] != nil || len(value["errors"].(map[string]any)) < 2 {
		t.Fatal("failed query reported backups disabled", value)
	}
	if _, err := b.Invoke(context.Background(), "room_backups", aicontrol.Arguments{RoomID: "other"}, "r"); !errors.Is(err, aicontrol.ErrDenied) {
		t.Fatal("room scope bypass", err)
	}
}

func TestAIQueriesUseDomainPlayerAndModFacts(t *testing.T) {
	f := &aiQueryFixture{refreshErr: errors.New("one Agent offline")}
	b := &AIBackend{queries: AIPlatformQueries{Players: f, Mods: f, ModMetadata: f, Automation: f}}
	raw, err := b.Invoke(context.Background(), "room_players", aicontrol.Arguments{RoomID: "r"}, "r")
	if err != nil || f.refreshes != 1 || !strings.Contains(string(raw), `"currentPresenceAvailable":false`) || !strings.Contains(string(raw), `"staleOnline":1`) || !strings.Contains(string(raw), "one Agent offline") || !strings.Contains(string(raw), `"worldConfirmed":false`) || !strings.Contains(string(raw), "World counts are local entities only") {
		t.Fatal(string(raw), err)
	}
	raw, err = b.Invoke(context.Background(), "room_mods", aicontrol.Arguments{RoomID: "r", Limit: 1}, "r")
	if err != nil || !strings.Contains(string(raw), `"hasMore":true`) || !strings.Contains(string(raw), `"installed":false`) || !strings.Contains(string(raw), "测试模组") || strings.Contains(string(raw), "关闭的模组") {
		t.Fatal(string(raw), err)
	}
	if f.metadataReads != 1 {
		t.Fatal("mod names did not use the shared cache", f.metadataReads)
	}
	raw, err = b.Invoke(context.Background(), "room_mods", aicontrol.Arguments{RoomID: "r", Limit: 1, Offset: 1}, "r")
	if err != nil || !strings.Contains(string(raw), `"hasMore":false`) || !strings.Contains(string(raw), "关闭的模组") {
		t.Fatal(string(raw), err)
	}
}

func TestAIPerformanceFollowsAppliedInstallationAndRetainsMissingMetrics(t *testing.T) {
	router, _, _, roomService := newRoomHandlerApp(t)
	response := performJSON(router, http.MethodPost, "/api/v2/rooms", map[string]any{"directoryName": "perf", "name": "资源测试", "gameMode": "survival", "maxPlayers": 6, "includeCaves": true}, nil, "")
	assertStatus(t, response, 201)
	roomID := responseData(t, response)["id"].(string)
	worlds, _ := roomService.Worlds(roomID)
	now := time.Now().UTC()
	local, remote, unrelated := runtimeObservationHTTPInventory(now), runtimeObservationHTTPInventory(now), runtimeObservationHTTPInventory(now)
	local.Target.ID, local.Target.Name, local.Target.Kind = "local", "本机", agents.RuntimeKindLocal
	remote.Target.ID, remote.Target.Name = "agent:remote", "远端"
	unrelated.Target.ID, unrelated.Target.Name = "agent:other", "无关机器"
	local.Inventory.Processes = []shared.ShardProcessReport{{PID: 11, Cluster: "perf", Shard: worlds[0].DirectoryName, CPUPercent: 23.5, RSSBytes: 500}, {PID: 99, Cluster: "other-room", Shard: worlds[0].DirectoryName, RSSBytes: 999}}
	remote.Inventory.Processes = []shared.ShardProcessReport{{PID: 22, Cluster: "perf", Shard: worlds[1].DirectoryName, CPUPercent: 42, RSSBytes: 700}}
	source := &runtimeObservationHTTPSource{items: []agents.RuntimeTargetInventory{local, remote, unrelated}}
	coordinator, err := runtimeobservation.New(source)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	fixture := newAIOverviewFixture()
	fixture.topology.RoomID, fixture.topology.Placements = roomID, nil
	for i, world := range worlds {
		target := "local"
		if i == 1 {
			target = "agent:remote"
		}
		fixture.topology.Placements = append(fixture.topology.Placements, topology.Placement{WorldID: world.ID, WorldName: world.Name, AppliedTargetID: target, AppliedInstallationID: "default"})
		fixture.statuses[world.ID] = shared.ShardRuntimeStatus{State: "running"}
	}
	service, err := runtimeoverview.New(fixture, fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	b := &AIBackend{rooms: &RoomHandler{rooms: roomService}, overview: &RuntimeObservabilityHandler{overview: service, observations: coordinator}}
	read := func() string {
		t.Helper()
		raw, err := b.Invoke(context.Background(), "room_performance", aicontrol.Arguments{RoomID: roomID}, roomID)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	value := read()
	if !strings.Contains(value, `"cpuPercent":23.5`) || !strings.Contains(value, `"memoryRssBytes":700`) || strings.Contains(value, `"pid":99`) || strings.Contains(value, "无关机器") {
		t.Fatal(value)
	}
	source.mu.Lock()
	source.items[1].Target.Online = false
	source.mu.Unlock()
	value = read()
	if strings.Contains(value, `"memoryRssBytes":700`) || !strings.Contains(value, "inventory_unavailable_or_stale") {
		t.Fatal("offline process presented as live", value)
	}
	source.mu.Lock()
	source.items[0].Target.Config.InstallationID = "wrong-installation"
	source.mu.Unlock()
	if value = read(); strings.Contains(value, `"pid":11`) {
		t.Fatal("fell back to another installation", value)
	}
}
