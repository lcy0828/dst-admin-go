package modcontrol

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jinzhu/gorm"
)

func TestConfigurationModeSurvivesStoreReopenAndDoesNotWriteWorldFiles(t *testing.T) {
	fixture := newSnapshotFixture(t)
	db, err := gorm.Open("sqlite3", filepath.Join(t.TempDir(), "modes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewConfigurationModeStore(db, "test_")
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	coordinator := &snapshotCoordinator{source: fixture.source}
	service, err := NewService(fixture.source, fixture.mods, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureConfigurationModes(store)
	if err := service.SetConfigurationMode(context.Background(), fixture.room.ID, "100", "separate"); err != nil {
		t.Fatal(err)
	}
	service.ConfigureConfigurationModes(NewConfigurationModeStore(db, "test_"))
	profile, err := service.RoomProfile(context.Background(), fixture.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range profile.Items {
		if item.ModID == "100" {
			found = true
			if item.ConfigurationMode != "separate" {
				t.Fatalf("lost mode: %#v", item)
			}
		}
	}
	if !found {
		t.Fatal("fixture Mod missing")
	}
	if len(coordinator.published) != 0 {
		t.Fatal("mode change published game configuration")
	}
	if err := service.SetConfigurationMode(context.Background(), fixture.room.ID, "100", "invalid"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}
