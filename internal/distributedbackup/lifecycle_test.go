package distributedbackup

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/roomops"

	"github.com/google/uuid"
)

func createLifecycleSet(t *testing.T, fixture distributedBackupFixture, kind string) Set {
	t.Helper()
	value, err := fixture.coordinator.Create(context.Background(), "room", "test backup", kind, "")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestBackupExportContainsPortableRoomAndRejectsTampering(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	value := createLifecycleSet(t, fixture, "manual")
	file, _, err := fixture.coordinator.Export(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	info, _ := file.Stat()
	archive, err := zip.NewReader(file, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		contents[entry.Name] = string(raw)
	}
	if contents["cluster.ini"] == "" || contents["Master/save/session/SESSION/0000000001"] != "master-v1" || contents["Caves/save/session/SESSION/0000000001"] != "caves-v1" {
		t.Fatalf("export lost room data: %v", contents)
	}
	source, _ := fixture.coordinator.partPath(value.Parts[0])
	if err := os.WriteFile(source, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if file, _, err := fixture.coordinator.Export(context.Background(), value.ID); !errors.Is(err, ErrIntegrity) {
		if file != nil {
			file.Close()
			os.Remove(file.Name())
		}
		t.Fatalf("tampered export: %v", err)
	}
}

func TestBackupDeleteProtectsRecoveryAndNeverTouchesSaves(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	value := createLifecycleSet(t, fixture, "protection")
	source := filepath.Join(fixture.masterRoot, "Cluster_1", "Master", "save", "session", "SESSION", "0000000001")
	before, _ := os.ReadFile(source)
	if _, err := fixture.coordinator.Delete(context.Background(), value.ID, "wrong"); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatal(err)
	}
	operation, err := fixture.store.CreateOperation(Operation{ID: uuid.NewString(), SetID: "other", ProtectionSetID: value.ID, RoomID: value.RoomID, Status: OperationRecoveryRequired, Kind: "restore", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.Delete(context.Background(), value.ID, value.Name); !errors.Is(err, ErrInUse) {
		t.Fatalf("recovery backup removed: %v", err)
	}
	operation.Status = OperationSucceeded
	if _, err := fixture.store.SaveOperation(operation); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.Delete(context.Background(), value.ID, value.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.GetSet(value.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.coordinator.root, "sets", value.ID)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || string(after) != string(before) {
		t.Fatalf("save changed: %v", err)
	}
}

func TestBackupRetentionKeepsManualProtectionAndNewestSnapshots(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	manual := createLifecycleSet(t, fixture, "manual")
	protection := createLifecycleSet(t, fixture, "protection")
	old := createLifecycleSet(t, fixture, "snapshot")
	newer := createLifecycleSet(t, fixture, "snapshot")
	newest := createLifecycleSet(t, fixture, "snapshot")
	size, count, err := fixture.coordinator.PruneSnapshots(context.Background(), "room", 2)
	if err != nil || count != 1 || size != old.Size {
		t.Fatalf("prune: %d %d %v", size, count, err)
	}
	for _, value := range []Set{manual, protection, newer, newest} {
		if _, err := fixture.store.GetSet(value.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.GetSet(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err := fixture.coordinator.PruneSnapshots(context.Background(), "room", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
}

func TestInterruptedBackupDeletionRestoresUncommittedFiles(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	value := createLifecycleSet(t, fixture, "manual")
	directory, _ := fixture.coordinator.setDirectory(value.ID)
	tombstone := filepath.Join(fixture.coordinator.root, "sets", ".deleting-"+value.ID)
	if err := os.Rename(directory, tombstone); err != nil {
		t.Fatal(err)
	}
	if err := fixture.coordinator.recoverDeletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, tombstone); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.DeleteSet(value.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.coordinator.recoverDeletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestBackupDeleteRejectsSymlinkToSaveDirectory(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	value := createLifecycleSet(t, fixture, "manual")
	directory, _ := fixture.coordinator.setDirectory(value.ID)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.masterRoot, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.coordinator.Delete(context.Background(), value.ID, value.Name); !errors.Is(err, ErrIntegrity) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.masterRoot, "Cluster_1", "cluster.ini")); err != nil {
		t.Fatal(err)
	}
}

func TestBackupDeletionHonorsOtherOperationReferences(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	value := createLifecycleSet(t, fixture, "protection")
	fixture.coordinator.ConfigureDeletionGuard(func(candidate Set) error {
		if candidate.ID == value.ID {
			return ErrInUse
		}
		return nil
	})
	if _, err := fixture.coordinator.Delete(context.Background(), value.ID, value.Name); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	if _, err := fixture.store.GetSet(value.ID); err != nil {
		t.Fatal(err)
	}
	path, err := fixture.coordinator.partPath(value.Parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionDeadlineWhileWaitingForRoomPreservesSnapshots(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	old := createLifecycleSet(t, fixture, "snapshot")
	createLifecycleSet(t, fixture, "snapshot")
	_, unlock, err := roomops.Acquire(context.Background(), "room")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := fixture.coordinator.PruneSnapshots(ctx, "room", 1)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retention: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retention ignored the task deadline")
	}
	unlock()
	if _, err := fixture.store.GetSet(old.ID); err != nil {
		t.Fatal("canceled retention deleted a backup")
	}
	if _, err := os.Stat(filepath.Join(fixture.coordinator.root, "sets", old.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionCancellationDuringFinalDeletionFinishesCommitAndReportsCancellation(t *testing.T) {
	fixture := newDistributedBackupFixture(t)
	old := createLifecycleSet(t, fixture, "snapshot")
	newest := createLifecycleSet(t, fixture, "snapshot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.coordinator.ConfigureDeletionGuard(func(Set) error {
		cancel()
		return nil
	})
	size, count, err := fixture.coordinator.PruneSnapshots(ctx, "room", 1)
	if !errors.Is(err, context.Canceled) || count != 1 || size != old.Size {
		t.Fatalf("canceled retention: size=%d count=%d err=%v", size, count, err)
	}
	if _, err := fixture.store.GetSet(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deletion did not commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.coordinator.root, "sets", old.ID)); !os.IsNotExist(err) {
		t.Fatalf("committed deletion left an artifact: %v", err)
	}
	if _, err := fixture.store.GetSet(newest.ID); err != nil {
		t.Fatal(err)
	}
}
