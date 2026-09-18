package automation

import (
	"context"
	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"dont/internal/runtimeguard"
	"errors"
)

type DistributedBackupCreator interface {
	CreateWithMode(context.Context, string, string, string, string, string) (distributedbackup.Set, error)
	PruneSnapshots(context.Context, string, int) (int64, int, error)
}

// BackupRouter uses the same consistency coordinator for local and split rooms.
// Legacy ZIP retention remains available for existing snapshot policies.
type BackupRouter struct {
	BackupExecutor
	Distributed DistributedBackupCreator
}

func (r BackupRouter) Create(ctx context.Context, roomID, name string, kind backups.Kind, jobID string) (backups.Backup, error) {
	if r.Distributed == nil {
		return backups.Backup{}, errors.New("consistent backup coordinator is required")
	}
	created, err := r.Distributed.CreateWithMode(ctx, roomID, name, string(kind), jobID, distributedbackup.ModeAutomatic)
	if err != nil {
		return backups.Backup{}, err
	}
	return backups.Backup{ID: created.ID, RoomID: roomID, Name: created.Name, Kind: kind, Status: string(created.Status), CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt}, nil
}

func (r BackupRouter) PruneSnapshots(ctx context.Context, roomID string, keep int) (int64, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	size, count, err := r.Distributed.PruneSnapshots(ctx, roomID, keep)
	if err != nil {
		return size, count, err
	}
	legacySize, legacyCount, err := r.BackupExecutor.PruneSnapshots(ctx, roomID, keep)
	if errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
		err = nil
	}
	return size + legacySize, count + legacyCount, err
}
