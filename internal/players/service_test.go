package players

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/configuration"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
)

type playerTestCatalog struct {
	room   rooms.Room
	worlds []rooms.World
}

func (c playerTestCatalog) Room(id string) (rooms.Room, error) {
	if id != c.room.ID {
		return rooms.Room{}, rooms.ErrRoomNotFound
	}
	return c.room, nil
}

func (c playerTestCatalog) World(roomID, worldID string) (rooms.World, error) {
	if roomID != c.room.ID {
		return rooms.World{}, rooms.ErrRoomNotFound
	}
	for _, world := range c.worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return rooms.World{}, rooms.ErrWorldNotFound
}

func (c playerTestCatalog) Worlds(roomID string) ([]rooms.World, error) {
	if roomID != c.room.ID {
		return nil, rooms.ErrRoomNotFound
	}
	return append([]rooms.World(nil), c.worlds...), nil
}

type playerTestRuntime struct {
	running map[string]bool
	local   *bool
}

func (r *playerTestRuntime) IsRunning(_ context.Context, roomName, worldName string) (bool, error) {
	return r.running[roomName+"/"+worldName], nil
}

func (r *playerTestRuntime) IsLocalPlacement(_, _ string) (bool, error) {
	if r.local == nil {
		return true, nil
	}
	return *r.local, nil
}

type playerTestSender struct {
	scripts []string
	err     error
}

func (s *playerTestSender) Send(_ context.Context, _, _ string, script string) error {
	s.scripts = append(s.scripts, script)
	return s.err
}

type playerTestAccess struct {
	values      configuration.AccessLists
	backupCount int
	readCount   int
	err         error
}

func (a *playerTestAccess) AccessLists(string) (configuration.AccessLists, error) {
	a.readCount++
	return a.values, a.err
}

func (a *playerTestAccess) ApplyAccess(_ context.Context, _ string, _ string, request configuration.AccessUpdateRequest) (configuration.ApplyResult, error) {
	if request.ExpectedRevision != a.values.Revision {
		return configuration.ApplyResult{}, configuration.ErrRevisionConflict
	}
	if len(request.Blocked) < len(a.values.Blocked) && request.Confirmation != "测试房间" {
		return configuration.ApplyResult{}, configuration.ErrConfirmationNeeded
	}
	a.values = configuration.AccessLists{Revision: "next", Admins: request.Admins, Blocked: request.Blocked, Whitelist: request.Whitelist}
	a.backupCount++
	return configuration.ApplyResult{ProtectionBackupID: "backup"}, nil
}

type playerTestProbe struct {
	items        []Observation
	historyItems []Observation
	err          error
	historyErr   error
}

type playerWorldProbe struct {
	items map[string][]Observation
	errs  map[string]error
}

func (p *playerWorldProbe) Snapshot(_ context.Context, _, worldID string) ([]Observation, error) {
	return append([]Observation(nil), p.items[worldID]...), p.errs[worldID]
}

func (p *playerTestProbe) Snapshot(context.Context, string, string) ([]Observation, error) {
	return append([]Observation(nil), p.items...), p.err
}

func (p *playerTestProbe) HistorySnapshot(context.Context, string, string) ([]Observation, error) {
	return append([]Observation(nil), p.historyItems...), p.historyErr
}

func newPlayerTestService(t *testing.T) (*Service, *playerTestRuntime, *playerTestSender, *playerTestAccess, *playerTestProbe) {
	t.Helper()
	catalog := playerTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "地面", IsMaster: true},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "洞穴"},
		},
	}
	runtime := &playerTestRuntime{running: map[string]bool{"Cluster_1/Master": true, "Cluster_1/Caves": true}}
	sender := &playerTestSender{}
	access := &playerTestAccess{values: configuration.AccessLists{Revision: "current", Admins: []string{}, Blocked: []string{}, Whitelist: []string{}}}
	probe := &playerTestProbe{items: []Observation{{ID: "KU_ONE", Name: "Willow", Prefab: "willow", Age: 10}}}
	service, err := NewService(catalog, runtime, sender, access, newPlayerTestStore(t), probe)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC) }
	return service, runtime, sender, access, probe
}

func TestRefreshDoesNotMarkPlayersOfflineWhenProbeFails(t *testing.T) {
	service, runtime, _, _, probe := newPlayerTestService(t)
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	probe.err = errors.New("log unavailable")
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err == nil {
		t.Fatal("probe failure was ignored")
	}
	player, err := service.Player("room", "KU_ONE")
	if err != nil || !player.Online {
		t.Fatalf("probe failure incorrectly changed status: %#v err=%v", player, err)
	}
	probe.err = nil
	runtime.running["Cluster_1/Master"] = false
	previous, err := service.Player("room", "KU_ONE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	player, _ = service.Player("room", "KU_ONE")
	if player.Online {
		t.Fatal("stopped world still reports player online")
	}
	if !player.LastRefreshedAt.Equal(previous.LastRefreshedAt) {
		t.Fatalf("stopped refresh changed collection timestamp: before=%s after=%s", previous.LastRefreshedAt, player.LastRefreshedAt)
	}
}

func TestRefreshPresenceRequiresFreshTelemetryFromEveryRunningWorld(t *testing.T) {
	service, _, _, _, probe := newPlayerTestService(t)
	result, err := service.RefreshPresence(context.Background(), "room")
	if err != nil || !result.Fresh || result.Online != 1 || result.StaleOnline != 0 || len(result.Worlds) != 2 {
		t.Fatalf("fresh presence = %#v, error = %v", result, err)
	}
	probe.err = errors.New("runtime telemetry unavailable")
	result, err = service.RefreshPresence(context.Background(), "room")
	if err != nil || result.Fresh || result.Online != 1 || result.StaleOnline != 1 || len(result.Warnings) == 0 {
		t.Fatalf("stale presence = %#v, error = %v", result, err)
	}
}

func TestRefreshPresenceTreatsConfirmedStoppedWorldsAsEmpty(t *testing.T) {
	service, runtime, _, _, probe := newPlayerTestService(t)
	runtime.running["Cluster_1/Master"] = false
	runtime.running["Cluster_1/Caves"] = false
	probe.err = errors.New("probe must not be required for stopped worlds")
	result, err := service.RefreshPresence(context.Background(), "room")
	if err != nil || !result.Fresh || result.Online != 0 || result.StaleOnline != 0 {
		t.Fatalf("stopped presence = %#v, error = %v", result, err)
	}
}

func TestOnlineCountDoesNotReadAccessLists(t *testing.T) {
	service, _, _, access, _ := newPlayerTestService(t)
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	count, err := service.OnlineCount("room")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("online count = %d", count)
	}
	if access.readCount != 0 {
		t.Fatalf("online count unexpectedly read access lists %d time(s)", access.readCount)
	}
}

func TestListCanSkipAccessListsForDashboardSummary(t *testing.T) {
	service, _, _, access, _ := newPlayerTestService(t)
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err := service.List("room", ListFilter{Limit: 25, SkipAccessLists: true})
	if err != nil {
		t.Fatal(err)
	}
	if access.readCount != 0 {
		t.Fatalf("lightweight list unexpectedly read access lists %d time(s)", access.readCount)
	}
	if list.Total != 1 || list.Online != 1 || len(list.Items) != 1 {
		t.Fatalf("lightweight player list lost summary data: %#v", list)
	}
	if list.AccessListsAvailable || list.Items[0].AccessListsAvailable || list.Items[0].Banned {
		t.Fatalf("lightweight list presented omitted access data as available: %#v", list)
	}
}

func TestRemotePlayerReadsUsePlacementAwareAccessManager(t *testing.T) {
	service, runtime, _, access, probe := newPlayerTestService(t)
	remote := false
	runtime.local = &remote
	access.values = configuration.AccessLists{Revision: "remote", Blocked: []string{"KU_REMOTE"}}
	probe.items = []Observation{{ID: "KU_REMOTE", Name: "Remote Willow", Prefab: "willow", Age: 12}}

	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	list, err := service.List("room", ListFilter{Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	if access.readCount != 1 {
		t.Fatalf("remote list access reads = %d", access.readCount)
	}
	if !list.AccessListsAvailable || len(list.Items) != 1 || list.Items[0].ID != "KU_REMOTE" || !list.Items[0].Banned {
		t.Fatalf("unexpected remote player list: %#v", list)
	}
	player, err := service.Player("room", "KU_REMOTE")
	if err != nil {
		t.Fatal(err)
	}
	if !player.AccessListsAvailable || player.AccessWarning != "" || !player.Banned || access.readCount != 2 {
		t.Fatalf("unexpected remote player: %#v reads=%d", player, access.readCount)
	}
}

func TestRefreshWorldsCommitsSuccessfulShardsAndPreservesFailedShard(t *testing.T) {
	catalog := playerTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "测试房间", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "地面", IsMaster: true},
			{ID: "caves", RoomID: "room", DirectoryName: "Caves", Name: "洞穴"},
		},
	}
	runtime := &playerTestRuntime{running: map[string]bool{"Cluster_1/Master": true, "Cluster_1/Caves": true}}
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{
		{WorldID: "master", WorldName: "地面", ObservedAt: now, Observations: []Observation{{ID: "KU_MASTER_OLD", Name: "Old Master"}}},
		{WorldID: "caves", WorldName: "洞穴", ObservedAt: now, Observations: []Observation{{ID: "KU_CAVES", Name: "Caves"}}},
	}); err != nil {
		t.Fatal(err)
	}
	probe := &playerWorldProbe{
		items: map[string][]Observation{"master": {{ID: "KU_MASTER_NEW", Name: "New Master"}}},
		errs:  map[string]error{"caves": errors.New("caves snapshot unavailable")},
	}
	access := &playerTestAccess{values: configuration.AccessLists{Revision: "current"}}
	service, err := NewService(catalog, runtime, &playerTestSender{}, access, store, probe)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }

	outcomes, err := service.RefreshWorlds(context.Background(), "room", []string{"master", "caves"})
	if err != nil || len(outcomes) != 2 || outcomes[0].Err != nil || outcomes[1].Err == nil {
		t.Fatalf("unexpected partial refresh: outcomes=%#v err=%v", outcomes, err)
	}
	masterOld, err := store.Get("room", "KU_MASTER_OLD")
	if err != nil || masterOld.Online {
		t.Fatalf("successful shard did not mark missing player offline: player=%#v err=%v", masterOld, err)
	}
	masterNew, err := store.Get("room", "KU_MASTER_NEW")
	if err != nil || !masterNew.Online || masterNew.WorldID != "master" {
		t.Fatalf("successful shard was not committed: player=%#v err=%v", masterNew, err)
	}
	caves, err := store.Get("room", "KU_CAVES")
	if err != nil || !caves.Online || caves.WorldID != "caves" || caves.PresenceStatus != FreshnessStale {
		t.Fatalf("failed shard state was cleared: player=%#v err=%v", caves, err)
	}
}

func TestRefreshWorldsPreservesLivePlayersWhenRuntimeRefreshIsDeferred(t *testing.T) {
	catalog := playerTestCatalog{
		room: rooms.Room{ID: "room", DirectoryName: "Cluster_1", Name: "Room", Managed: true},
		worlds: []rooms.World{
			{ID: "master", RoomID: "room", DirectoryName: "Master", Name: "地面", IsMaster: true},
		},
	}
	runtime := &playerTestRuntime{running: map[string]bool{"Cluster_1/Master": true}}
	store := newPlayerTestStore(t)
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	if err := store.ReplaceRoomSnapshots("room", []worldSnapshot{{
		WorldID: "master", WorldName: "地面", ObservedAt: now,
		Observations: stampNativeObservations([]Observation{{ID: "KU_CURRENT", Name: "Current player"}}, now),
	}}); err != nil {
		t.Fatal(err)
	}
	probe := &playerWorldProbe{errs: map[string]error{
		"master": errors.Join(dstruntime.ErrSnapshotStale, dstruntime.ErrRuntimeRefreshDeferred),
	}}
	service, err := NewService(catalog, runtime, &playerTestSender{}, &playerTestAccess{}, store, probe)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }

	outcomes, err := service.RefreshWorlds(context.Background(), "room", []string{"master"})
	if err != nil || len(outcomes) != 1 || outcomes[0].Err != nil || !outcomes[0].Deferred {
		t.Fatalf("deferred outcomes=%#v error=%v", outcomes, err)
	}
	player, err := store.Get("room", "KU_CURRENT")
	if err != nil || !player.Online || player.PresenceStatus != FreshnessLive {
		t.Fatalf("preserved player=%#v error=%v", player, err)
	}
}

func TestRefreshRestoresHistoricalPlayersWhileWorldIsStopped(t *testing.T) {
	service, runtime, _, _, probe := newPlayerTestService(t)
	probe.historyItems = []Observation{{ID: "KU_HISTORY", Name: "Wendy", Prefab: "wendy", Age: 12}}
	runtime.running["Cluster_1/Master"] = false
	result, err := service.RefreshWorld(context.Background(), "room", "master")
	if err != nil {
		t.Fatal(err)
	}
	if result.Running || !strings.Contains(result.Message, "已恢复 1 个历史玩家") {
		t.Fatalf("unexpected stopped refresh result: %#v", result)
	}
	if result.Status != FreshnessStale || result.ObservedAt != nil {
		t.Fatalf("stopped refresh was presented as a live observation: %#v", result)
	}
	player, err := service.Player("room", "KU_HISTORY")
	if err != nil || player.Online || player.WorldID != "master" {
		t.Fatalf("historical player was not restored offline: player=%#v err=%v", player, err)
	}
}

func TestPlayerActionsEnforceWorldAndPersistBanList(t *testing.T) {
	service, _, sender, access, _ := newPlayerTestService(t)
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionKick, ActionRequest{WorldID: "caves", Confirmation: "KU_ONE"}); err == nil {
		t.Fatal("cross-world kick was accepted")
	}
	message := `hello "); c_shutdown(); --`
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionAnnounce, ActionRequest{WorldID: "master", Message: message}); err != nil {
		t.Fatal(err)
	}
	if len(sender.scripts) != 1 || strings.Contains(sender.scripts[0], `c_announce("[给 Willow] hello "); c_shutdown()`) {
		t.Fatalf("announcement was not safely quoted: %q", sender.scripts)
	}
	result, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionBan, ActionRequest{
		WorldID: "master", Confirmation: "测试房间", Reason: "破坏建筑", Duration: "1h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(access.values.Blocked, "KU_ONE") || result.ProtectionBackupID != "backup" || access.backupCount != 1 {
		t.Fatalf("ban was not persisted with protection: result=%#v access=%#v", result, access)
	}
	player, err := service.Player("room", "KU_ONE")
	if err != nil || player.BanReason != "破坏建筑" || player.BanExpiresAt == nil {
		t.Fatalf("temporary ban details were not persisted: player=%#v err=%v", player, err)
	}
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionUnban, ActionRequest{WorldID: "master", Confirmation: "wrong"}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("invalid unban confirmation accepted: %v", err)
	}
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionUnban, ActionRequest{WorldID: "master", Confirmation: "测试房间"}); err != nil {
		t.Fatal(err)
	}
	if contains(access.values.Blocked, "KU_ONE") || access.backupCount != 2 {
		t.Fatalf("unban did not update access list: %#v", access.values)
	}
}

func TestTemporaryBanExpiresAndPlayerCommandsAreFixedTemplates(t *testing.T) {
	service, _, sender, access, _ := newPlayerTestService(t)
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionGodMode, ActionRequest{WorldID: "master", Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if len(sender.scripts) != 1 {
		t.Fatalf("god mode did not use the fixed server template: %q", sender.scripts)
	}
	for _, fragment := range []string{"_dst_admin_god_task", "SetInvincible(true)", "SetPercent(1)", "SetTemperature(35)", "WATERPROOFNESS_ABSOLUTE", "ClearCollidesWith(COLLISION.LIMITS)", `AddTag("dst_admin_god_mode")`} {
		if !strings.Contains(sender.scripts[0], fragment) {
			t.Fatalf("god mode template missing %q: %s", fragment, sender.scripts[0])
		}
	}
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionCreativeMode, ActionRequest{WorldID: "master"}); err == nil {
		t.Fatal("creative mode accepted a missing enabled value")
	}
	if _, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionBan, ActionRequest{
		WorldID: "master", Confirmation: "测试房间", Reason: "临时封禁", Duration: "1h",
	}); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 8, 11, 1, 0, 0, time.UTC) }
	if err := service.ExpireBans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if contains(access.values.Blocked, "KU_ONE") {
		t.Fatal("expired temporary ban remains in the blocklist")
	}
	bans, err := service.store.Bans("room")
	if err != nil || len(bans) != 0 {
		t.Fatalf("expired ban metadata remains: %#v err=%v", bans, err)
	}
}
