package automation

import (
	"context"
	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"dont/internal/runtimeguard"
	"errors"
	"testing"
)

type scheduledLocalBackup struct{ err error }

func (b scheduledLocalBackup) Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error) {
	return backups.Backup{Name: "local"}, b.err
}
func (scheduledLocalBackup) PruneSnapshots(string, int) (int64, int, error) { return 0, 0, nil }

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
	for _, cause := range []error{nil, runtimeguard.ErrRemoteMutationUnavailable, errors.New("disk full")} {
		remote := &scheduledRemoteBackup{}
		router := BackupRouter{BackupExecutor: scheduledLocalBackup{err: cause}, Distributed: remote}
		_, err := router.Create(context.Background(), "split-room", "snapshot", backups.KindSnapshot, "job")
		if errors.Is(cause, runtimeguard.ErrRemoteMutationUnavailable) {
			if err != nil || remote.calls != 1 || remote.room != "split-room" || remote.mode != distributedbackup.ModeAutomatic || remote.job != "job" {
				t.Fatalf("remote=%#v err=%v", remote, err)
			}
		} else if !errors.Is(err, cause) || remote.calls != 0 {
			t.Fatalf("unexpected fallback: %#v %v", remote, err)
		}
	}
}
