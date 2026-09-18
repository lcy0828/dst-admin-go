package configuration

import (
	"context"
	"fmt"
	"testing"

	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"dont/internal/runtimeguard"
)

type hybridLocalBackup struct{ err error }

func (b hybridLocalBackup) Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error) {
	if b.err != nil {
		return backups.Backup{}, b.err
	}
	return backups.Backup{ID: "local-backup"}, nil
}

type hybridDistributedBackup struct {
	calls int
	mode  string
}

func (b *hybridDistributedBackup) CreateWithMode(_ context.Context, roomID, name, kind, sourceJobID, mode string) (distributedbackup.Set, error) {
	b.calls++
	b.mode = mode
	return distributedbackup.Set{ID: "distributed-backup", RoomID: roomID, Name: name, Kind: kind, Status: distributedbackup.StatusVerified}, nil
}

func TestHybridBackupCreatorUsesDistributedBackupOnlyForRemotePlacement(t *testing.T) {
	distributed := &hybridDistributedBackup{}
	creator, err := NewHybridBackupCreator(hybridLocalBackup{err: fmt.Errorf("wrapped: %w", runtimeguard.ErrRemoteMutationUnavailable)}, distributed)
	if err != nil {
		t.Fatal(err)
	}
	created, err := creator.Create(context.Background(), "room", "protection", backups.KindProtection, "job")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "distributed-backup" || distributed.calls != 1 || distributed.mode != distributedbackup.ModeAutomatic {
		t.Fatalf("backup=%#v calls=%d mode=%s", created, distributed.calls, distributed.mode)
	}

	distributed = &hybridDistributedBackup{}
	creator, err = NewHybridBackupCreator(hybridLocalBackup{}, distributed)
	if err != nil {
		t.Fatal(err)
	}
	created, err = creator.Create(context.Background(), "room", "protection", backups.KindProtection, "job")
	if err != nil || created.ID != "local-backup" || distributed.calls != 0 {
		t.Fatalf("local backup=%#v distributed calls=%d error=%v", created, distributed.calls, err)
	}
}

func TestHybridBackupCreatorRoutesRunningLocalRoomToCoordinator(t *testing.T) {
	distributed := &hybridDistributedBackup{}
	creator, err := NewHybridBackupCreator(hybridLocalBackup{err: backups.ErrConsistentBackupRequired}, distributed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creator.Create(context.Background(), "room", "protection", backups.KindProtection, "job"); err != nil {
		t.Fatal(err)
	}
	if distributed.calls != 1 || distributed.mode != distributedbackup.ModeAutomatic {
		t.Fatalf("running room did not use coordinated backup: %#v", distributed)
	}
}
