package distributedbackup

import (
	"context"
	"errors"
	"testing"

	"dont/internal/backups"
	"dont/internal/runtimeguard"
)

type legacyRemoteGuard struct{}

func (legacyRemoteGuard) RequireRoom(string) error { return runtimeguard.ErrRemoteMutationUnavailable }
func (legacyRemoteGuard) RequireWorld(string, string) error {
	return runtimeguard.ErrRemoteMutationUnavailable
}

func TestLegacyCreatorUsesLiveConsistencyForLocalAndSplitRooms(t *testing.T) {
	for _, local := range []bool{true, false} {
		fixture := newBackupPlacementFixture(t, local)
		installHotBarrierDrivers(t, fixture, []int64{41, 41})
		creator := LegacyCreator{Coordinator: fixture.coordinator}
		value, err := creator.Create(context.Background(), "room", "protection", backups.KindProtection, "job")
		if err != nil {
			t.Fatalf("local=%v: %v", local, err)
		}
		set, err := fixture.store.GetSet(value.ID)
		if err != nil || set.Mode != ModeHot || len(set.Parts) != 2 || value.SourceJobID != "job" {
			t.Fatalf("local=%v backup=%#v err=%v", local, set, err)
		}
		if err := fixture.coordinator.VerifyProtection(context.Background(), value.ID, "room", "job"); err != nil {
			t.Fatal(err)
		}
		assertControlRunning(t, fixture.master, "Master")
		assertControlRunning(t, fixture.caves, "Caves")
	}
}

func TestLegacyLocalOperationStillRejectsRemotePlacementBeforeBackup(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	creator := LegacyCreator{Coordinator: fixture.coordinator, Guard: legacyRemoteGuard{}}
	if _, err := creator.Create(context.Background(), "room", "protection", backups.KindProtection, "job"); !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
		t.Fatalf("local operation accepted remote placement: %v", err)
	}
	values, err := fixture.store.ListSets("room")
	if err != nil || len(values) != 0 {
		t.Fatalf("guard had backup side effects: %v %v", values, err)
	}
}
