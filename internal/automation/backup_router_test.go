package automation

import (
	"context"
	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"errors"
	"testing"
)

type scheduledLocalBackup struct{ err error }

func (b scheduledLocalBackup) Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error) {
	return backups.Backup{Name: "local"}, b.err
}
func (scheduledLocalBackup) PruneSnapshots(context.Context, string, int) (int64, int, error) {
	return 0, 0, nil
}

type scheduledRemoteBackup struct {
	calls           int
	room, mode, job string
}

func (b *scheduledRemoteBackup) CreateWithMode(_ context.Context, room, name, kind, job, mode string) (distributedbackup.Set, error) {
	b.calls++
	b.room, b.mode, b.job = room, mode, job
	return distributedbackup.Set{Name: name}, nil
}
func TestScheduledBackupsRouteLocalAndSplitRooms(t *testing.T) {
	for _, cause := range []error{nil, errors.New("legacy must never be called")} {
		remote := &scheduledRemoteBackup{}
		router := BackupRouter{BackupExecutor: scheduledLocalBackup{err: cause}, Distributed: remote}
		_, err := router.Create(context.Background(), "split-room", "snapshot", backups.KindSnapshot, "job")
		if err != nil || remote.calls != 1 || remote.room != "split-room" || remote.mode != distributedbackup.ModeAutomatic || remote.job != "job" {
			t.Fatalf("coordinator=%#v err=%v", remote, err)
		}
	}
}
func (*scheduledRemoteBackup) PruneSnapshots(context.Context, string, int) (int64, int, error) {
	return 0, 0, nil
}

type retentionBackupExecutor struct {
	createErr             error
	creates, prunes, keep int
}

func (b *retentionBackupExecutor) Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error) {
	b.creates++
	return backups.Backup{Name: "scheduled"}, b.createErr
}
func (b *retentionBackupExecutor) PruneSnapshots(_ context.Context, _ string, keep int) (int64, int, error) {
	b.prunes++
	b.keep = keep
	return 10, 1, nil
}

func TestScheduleRetentionRunsOnlyAfterSuccessfulBackup(t *testing.T) {
	for _, failed := range []bool{false, true} {
		backup := &retentionBackupExecutor{}
		if failed {
			backup.createErr = errors.New("save barrier failed")
		}
		executor := &DomainExecutor{backups: backup}
		_, err := executor.Execute(context.Background(), Task{RoomID: "room", Action: ActionBackupCreate, Parameters: map[string]interface{}{"keep": 3}}, "job")
		if failed {
			if err == nil || backup.prunes != 0 {
				t.Fatalf("pruned after failure: %#v %v", backup, err)
			}
		} else if err != nil || backup.prunes != 1 || backup.keep != 3 {
			t.Fatalf("retention not applied: %#v %v", backup, err)
		}
	}
	backup := &retentionBackupExecutor{}
	executor := &DomainExecutor{backups: backup}
	if _, err := executor.Execute(context.Background(), Task{RoomID: "room", Action: ActionBackupCreate, Parameters: map[string]interface{}{}}, "job"); err != nil || backup.prunes != 0 {
		t.Fatalf("legacy task gained retention: %#v %v", backup, err)
	}
	for _, keep := range []interface{}{0, 101, "all", 2.5} {
		if err := executor.Validate(Task{Action: ActionBackupCreate, Parameters: map[string]interface{}{"keep": keep}}); err == nil {
			t.Fatalf("accepted retention %v", keep)
		}
	}
}
