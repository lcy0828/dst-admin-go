package rooms

import (
	"errors"
	"os"
	"path/filepath"
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
		ClusterToken:  "token-value",
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
