package rooms

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorldInitializationFailureDoesNotPublishPartialRoom(t *testing.T) {
	service, root := newTestService(t)
	failure := errors.New("collector install failed")
	service.ConfigureWorldInitializer(func(root, name string) error {
		if name == "Caves" {
			return failure
		}
		return os.WriteFile(filepath.Join(root, name, "customcommands.lua"), []byte("-- collector"), 0600)
	})
	_, err := service.Create(CreateRequest{DirectoryName: "new", Name: "New", GameMode: "survival", MaxPlayers: 6, IncludeCaves: true})
	if !errors.Is(err, failure) {
		t.Fatalf("create error=%v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("partial room remains: %v %v", entries, err)
	}
}

func TestWorldInitializationFailurePreservesExistingRoom(t *testing.T) {
	service, root := newTestService(t)
	room, err := service.Create(CreateRequest{DirectoryName: "existing", Name: "Existing", GameMode: "survival", MaxPlayers: 6})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "existing", "Master", "save", "keep")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("existing save"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("collector install failed")
	service.ConfigureWorldInitializer(func(string, string) error { return failure })
	_, err = service.CreateWorld(room.ID, CreateWorldRequest{DirectoryName: "Caves", Type: "cave"})
	if !errors.Is(err, failure) {
		t.Fatalf("create error=%v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing save" {
		t.Fatalf("existing save changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "existing", "Caves")); !os.IsNotExist(err) {
		t.Fatalf("partial world published: %v", err)
	}
}
