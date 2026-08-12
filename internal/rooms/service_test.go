package rooms

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/go-ini/ini"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	db.SingularTable(true)
	db.LogMode(false)
	t.Cleanup(func() { _ = db.Close() })
	store := NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	root := t.TempDir()
	catalog, err := NewCatalog(root, store)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return NewService(catalog, store), root
}

func TestCreateDiscoverAdoptAndReadWorlds(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{
		DirectoryName: "summer_2026",
		Name:          "周末生存服",
		Description:   "测试房间",
		GameMode:      "endless",
		MaxPlayers:    12,
		PvP:           true,
		Password:      "secret",
		ClusterToken:  "token-value-12345",
		IncludeCaves:  true,
	})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	if !room.Managed || room.DirectoryName != "summer_2026" || room.Name != "周末生存服" || room.WorldCount != 2 {
		t.Fatalf("unexpected room: %#v", room)
	}
	if room.PasswordProtected != true || room.MaxPlayers != 12 || room.GameMode != "endless" || !room.PvP {
		t.Fatalf("room config was not preserved: %#v", room)
	}
	worlds, err := service.Worlds(room.ID)
	if err != nil {
		t.Fatalf("list worlds: %v", err)
	}
	if len(worlds) != 2 || worlds[0].Role != WorldRoleMaster || worlds[1].Role != WorldRoleCaves {
		t.Fatalf("unexpected worlds: %#v", worlds)
	}
	if mode := fileMode(t, filepath.Join(root, "summer_2026", "cluster_token.txt")); mode != 0600 {
		t.Fatalf("cluster token mode = %o, want 600", mode)
	}
	assertWorldGenerationDefaults(t, filepath.Join(root, "summer_2026", "Master", "leveldataoverride.lua"), "default", "default")
	assertWorldGenerationDefaults(t, filepath.Join(root, "summer_2026", "Caves", "leveldataoverride.lua"), "cave_default", "caves")
	rooms, err := service.List()
	if err != nil || len(rooms) != 1 || !rooms[0].Managed {
		t.Fatalf("list rooms = %#v, %v", rooms, err)
	}
}

func TestListTreatsUncreatedSaveRootAsEmpty(t *testing.T) {
	service, root := newTestService(t)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	items, err := service.List()
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("list missing save root = %#v, %v", items, err)
	}
}

func TestDiscoveredRoomRequiresExplicitAdoption(t *testing.T) {
	service, root := newTestService(t)
	roomPath := filepath.Join(root, "existing")
	if err := os.MkdirAll(filepath.Join(roomPath, "Master"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "cluster.ini"), []byte("[NETWORK]\ncluster_name = Existing\n[GAMEPLAY]\nmax_players = 6\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roomPath, "Master", "server.ini"), []byte("[SHARD]\nis_master = true\n[NETWORK]\nserver_port = 10999\n"), 0640); err != nil {
		t.Fatal(err)
	}
	rooms, err := service.List()
	if err != nil || len(rooms) != 1 || rooms[0].Managed {
		t.Fatalf("unexpected discovered rooms: %#v, %v", rooms, err)
	}
	adopted, err := service.Adopt(rooms[0].ID)
	if err != nil || !adopted.Managed {
		t.Fatalf("adopt room: %#v, %v", adopted, err)
	}
}

func TestManagedRoomLifecycleCoversEveryManagementTransition(t *testing.T) {
	service, _ := newTestService(t)
	events := make([]string, 0, 4)
	service.SetManagedRoomLifecycle(func(roomID string) {
		events = append(events, "managed:"+roomID)
	}, func(roomID string) {
		events = append(events, "unmanaged:"+roomID)
	})
	room, err := service.Create(CreateRequest{
		DirectoryName: "lifecycle_room", Name: "生命周期测试", GameMode: "survival", MaxPlayers: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Unadopt(room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Adopt(room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"managed:" + room.ID,
		"unmanaged:" + room.ID,
		"managed:" + room.ID,
		"unmanaged:" + room.ID,
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %#v, want %#v", events, want)
	}
}

func TestManagedRoomLifecycleSubscribersComposeAndWorldCreationNotifies(t *testing.T) {
	service, _ := newTestService(t)
	events := make([]string, 0, 4)
	service.SetManagedRoomLifecycle(func(roomID string) { events = append(events, "first:"+roomID) }, nil)
	service.AddManagedRoomLifecycle(func(roomID string) { events = append(events, "second:"+roomID) }, nil)
	service.AddWorldLifecycle(func(roomID, worldID string) { events = append(events, "world:"+roomID+":"+worldID) })
	room, err := service.Create(CreateRequest{DirectoryName: "composed", Name: "组合测试", GameMode: "survival", MaxPlayers: 6})
	if err != nil {
		t.Fatal(err)
	}
	world, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves2", Type: "cave"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first:" + room.ID, "second:" + room.ID, "world:" + room.ID + ":" + world.ID}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("lifecycle events = %#v, want %#v", events, want)
	}
}

func TestCreateAndRecoverablyDeleteWorld(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{
		DirectoryName: "world_crud", Name: "世界管理", GameMode: "survival", MaxPlayers: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves2", Type: "cave"})
	if err != nil {
		t.Fatal(err)
	}
	if world.Role != WorldRoleCaves || world.ServerPort != 11000 {
		t.Fatalf("created world = %#v", world)
	}
	config, err := ini.Load(filepath.Join(root, "world_crud", "Caves2", "server.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Section("SHARD").Key("id").MustInt(0) != 2 ||
		config.Section("STEAM").Key("authentication_port").MustInt(0) != 8768 ||
		config.Section("STEAM").Key("master_server_port").MustInt(0) != 27018 {
		t.Fatalf("allocated server.ini = %#v", config)
	}
	if _, err := service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: "wrong"}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("delete confirmation error = %v", err)
	}
	deleted, err := service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.World(room.ID, world.ID); !errors.Is(err, ErrWorldNotFound) {
		t.Fatalf("deleted world is still discoverable: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "world_crud", deleted.RecoveryName)); err != nil || !info.IsDir() {
		t.Fatalf("recovery directory missing: %v", err)
	}
}

func TestRecoverablyDeleteRoom(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{
		DirectoryName: "room_delete", Name: "待删除房间", GameMode: "survival", MaxPlayers: 6, IncludeCaves: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: "wrong"}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("delete confirmation error = %v", err)
	}
	deleted, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Room(room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("deleted room is still discoverable: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, deleted.RecoveryName)); err != nil || !info.IsDir() {
		t.Fatalf("room recovery directory missing: %v", err)
	}
	managed, err := service.store.IsManaged(room.ID)
	if err != nil || managed {
		t.Fatalf("deleted room is still managed: managed=%v err=%v", managed, err)
	}
}

func TestListRestoreAndPurgeRecoveries(t *testing.T) {
	service, _ := newTestService(t)
	managed := make([]string, 0, 2)
	worldsCreated := make([]string, 0, 1)
	service.AddManagedRoomLifecycle(func(roomID string) { managed = append(managed, roomID) }, nil)
	service.AddWorldLifecycle(func(roomID, worldID string) { worldsCreated = append(worldsCreated, roomID+":"+worldID) })
	room, err := service.Create(CreateRequest{
		DirectoryName: "recover_room", Name: "恢复测试", GameMode: "survival", MaxPlayers: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err := service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves2", Type: "cave"})
	if err != nil {
		t.Fatal(err)
	}
	deletedWorld, err := service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	worldRecoveryName := filepath.Base(deletedWorld.RecoveryName)
	worldRecoveries, err := service.ListWorldRecoveries(room.ID)
	if err != nil || len(worldRecoveries) != 1 || worldRecoveries[0].RecoveryName != worldRecoveryName || worldRecoveries[0].DirectoryName != "Caves2" {
		t.Fatalf("world recoveries = %#v, %v", worldRecoveries, err)
	}
	restoredWorld, err := service.RestoreWorld(room.ID, worldRecoveryName)
	if err != nil || restoredWorld.ID != world.ID {
		t.Fatalf("restore world = %#v, %v", restoredWorld, err)
	}
	if len(worldsCreated) != 2 || worldsCreated[1] != room.ID+":"+world.ID {
		t.Fatalf("world lifecycle = %#v", worldsCreated)
	}
	deletedWorld, err = service.DeleteWorld(room.ID, world.ID, DeleteWorldRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	worldRecoveryName = filepath.Base(deletedWorld.RecoveryName)
	if err := service.PurgeWorldRecovery(room.ID, worldRecoveryName, PurgeRecoveryRequest{Confirmation: "wrong"}); !errors.Is(err, ErrRecoveryConfirmation) {
		t.Fatalf("purge world confirmation error = %v", err)
	}
	if err := service.PurgeWorldRecovery(room.ID, worldRecoveryName, PurgeRecoveryRequest{Confirmation: worldRecoveryName}); err != nil {
		t.Fatal(err)
	}
	worldRecoveries, err = service.ListWorldRecoveries(room.ID)
	if err != nil || len(worldRecoveries) != 0 {
		t.Fatalf("world recoveries after purge = %#v, %v", worldRecoveries, err)
	}

	deletedRoom, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	roomRecoveryName := filepath.Base(deletedRoom.RecoveryName)
	roomRecoveries, err := service.ListRoomRecoveries()
	if err != nil || len(roomRecoveries) != 1 || roomRecoveries[0].RecoveryName != roomRecoveryName || roomRecoveries[0].DisplayName != room.Name {
		t.Fatalf("room recoveries = %#v, %v", roomRecoveries, err)
	}
	restoredRoom, err := service.RestoreRoom(roomRecoveryName)
	if err != nil || restoredRoom.ID != room.ID || !restoredRoom.Managed {
		t.Fatalf("restore room = %#v, %v", restoredRoom, err)
	}
	if len(managed) != 2 || managed[1] != room.ID {
		t.Fatalf("managed lifecycle = %#v", managed)
	}
	deletedRoom, err = service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	roomRecoveryName = filepath.Base(deletedRoom.RecoveryName)
	if err := service.PurgeRoomRecovery(roomRecoveryName, PurgeRecoveryRequest{Confirmation: roomRecoveryName}); err != nil {
		t.Fatal(err)
	}
	roomRecoveries, err = service.ListRoomRecoveries()
	if err != nil || len(roomRecoveries) != 0 {
		t.Fatalf("room recoveries after purge = %#v, %v", roomRecoveries, err)
	}
}

func TestRecoveryRejectsUnsafeNamesSymlinksAndRestoreCollisions(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{
		DirectoryName: "collision", Name: "冲突测试", GameMode: "survival", MaxPlayers: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := service.DeleteRoom(room.ID, DeleteRoomRequest{Confirmation: room.Name})
	if err != nil {
		t.Fatal(err)
	}
	recoveryName := filepath.Base(deleted.RecoveryName)
	if err := os.MkdirAll(filepath.Join(root, room.DirectoryName), 0750); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RestoreRoom(recoveryName); !errors.Is(err, ErrRoomExists) {
		t.Fatalf("restore collision error = %v", err)
	}
	if _, err := service.RestoreRoom("../" + recoveryName); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe recovery error = %v", err)
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		linkName := "1234567890123456789-linked"
		if err := os.Symlink(outside, filepath.Join(root, ".dst-admin-trash", linkName)); err != nil {
			t.Fatal(err)
		}
		if _, err := service.RestoreRoom(linkName); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("symlink recovery error = %v", err)
		}
	}
}

func TestRejectsTraversalNonCanonicalIDsAndEscapingSymlinks(t *testing.T) {
	service, root := newTestService(t)
	badID := EncodeID("../outside")
	if _, err := service.Room(badID); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("traversal id error = %v, want ErrInvalidID", err)
	}
	if _, err := service.Room(EncodeID("valid") + "="); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("noncanonical id error = %v, want ErrInvalidID", err)
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "cluster.ini"), []byte("[NETWORK]\ncluster_name=outside\n"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}
		rooms, err := service.List()
		if err != nil || len(rooms) != 0 {
			t.Fatalf("escaping symlink was discovered: %#v, %v", rooms, err)
		}
	}
}

func TestCreateValidationDoesNotLeavePartialRoom(t *testing.T) {
	service, root := newTestService(t)
	_, err := service.Create(CreateRequest{DirectoryName: "../bad", Name: "", GameMode: "invalid", MaxPlayers: 0})
	var validation *ValidationError
	if !errors.As(err, &validation) || len(validation.Fields) < 4 {
		t.Fatalf("validation error = %#v", err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("validation left files: %#v, %v", entries, readErr)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func assertWorldGenerationDefaults(t *testing.T, path, taskSet, startLocation string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][]byte{[]byte(`task_set="` + taskSet + `"`), []byte(`start_location="` + startLocation + `"`), []byte(`required_prefabs={ "multiplayer_portal" }`)} {
		if !bytes.Contains(data, expected) {
			t.Fatalf("%s does not contain %q", path, expected)
		}
	}
}
