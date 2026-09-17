package dstruntime

import (
	"testing"

	"dont/internal/rooms"
	"github.com/jinzhu/gorm"
	_ "github.com/mattn/go-sqlite3"
)

func TestNewRoomAndWorldContainCollectorBeforeProvisioning(t *testing.T) {
	db, err := gorm.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := rooms.NewStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	catalog, err := rooms.NewCatalog(root, store)
	if err != nil {
		t.Fatal(err)
	}
	service := rooms.NewService(catalog, store)
	manager, err := NewManager(root, service)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureWorldInitializer(manager.InitializeWorld)
	room, err := service.Create(rooms.CreateRequest{DirectoryName: "new-room", Name: "New", GameMode: "survival", MaxPlayers: 6, IncludeCaves: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateWorld(room.ID, rooms.CreateWorldRequest{DirectoryName: "Extra", Type: "cave"}); err != nil {
		t.Fatal(err)
	}
	statuses, err := manager.StatusRoom(room.ID)
	if err != nil || len(statuses) != 3 {
		t.Fatalf("statuses=%+v err=%v", statuses, err)
	}
	for _, status := range statuses {
		if status.State != InstallStateInstalled {
			t.Fatalf("new world has no collector: %+v", status)
		}
	}
	bundle, err := service.ProvisionBundle(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, world := range bundle.Worlds {
		files := make(map[string]bool)
		for _, file := range world.Files {
			files[file.Name] = true
		}
		for _, name := range []string{"customcommands.lua", "dst-admin/manifest.json", "dst-admin/bootstrap.lua", "dst-admin/worldstate.lua", "dst-admin/telemetry.lua"} {
			if !files[name] {
				t.Fatalf("%s missing %s in remote provision bundle", world.World.Name, name)
			}
		}
	}
}
