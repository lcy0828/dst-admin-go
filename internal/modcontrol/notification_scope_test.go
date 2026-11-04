package modcontrol

import (
	"context"
	"testing"
)

func TestInstallationNotificationScopeExcludesDisabledAndOtherPlacement(t *testing.T) {
	f := newSnapshotFixture(t)
	s := &Service{source: f.source}
	ctx := context.Background()
	local, err := s.InstallationEnabledModRooms(ctx, "local", "default", []string{"100", "200", "999"})
	if err != nil || len(local[f.room.ID]) != 1 || local[f.room.ID][0] != "100" {
		t.Fatal("wrong local installation notification scope", local, err)
	}
	remote, err := s.InstallationEnabledModRooms(ctx, "target-2", "install-2", []string{"100", "200", "999"})
	if err != nil || len(remote) != 0 {
		t.Fatal("disabled or local decoy Mod was marked as affected", remote, err)
	}
	f.driver.overrides[f.room.DirectoryName+"\x00"+f.caves.DirectoryName] = []byte(`return { ["workshop-200"] = { enabled = true } }`)
	remote, err = s.InstallationEnabledModRooms(ctx, "target-2", "install-2", []string{"100", "200", "999"})
	if err != nil || len(remote[f.room.ID]) != 1 || remote[f.room.ID][0] != "200" {
		t.Fatal("enabled remote Mod notification lost", remote, err)
	}
}
