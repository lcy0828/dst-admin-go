package players

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/configuration"
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

type playerTestRuntime struct{ running map[string]bool }

func (r *playerTestRuntime) IsRunning(_ context.Context, roomName, worldName string) (bool, error) {
	return r.running[roomName+"/"+worldName], nil
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
}

func (a *playerTestAccess) AccessLists(string) (configuration.AccessLists, error) {
	return a.values, nil
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
	items []Observation
	err   error
}

func (p *playerTestProbe) Snapshot(context.Context, string, string) ([]Observation, error) {
	return append([]Observation(nil), p.items...), p.err
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
	if _, err := service.RefreshWorld(context.Background(), "room", "master"); err != nil {
		t.Fatal(err)
	}
	player, _ = service.Player("room", "KU_ONE")
	if player.Online {
		t.Fatal("stopped world still reports player online")
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
	result, err := service.Act(context.Background(), "job", "room", "KU_ONE", ActionBan, ActionRequest{WorldID: "master", Confirmation: "测试房间"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(access.values.Blocked, "KU_ONE") || result.ProtectionBackupID != "backup" || access.backupCount != 1 {
		t.Fatalf("ban was not persisted with protection: result=%#v access=%#v", result, access)
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
