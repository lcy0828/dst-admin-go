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
}

// BackupRouter keeps local snapshots and routes remote rooms to their applied
// placements. The coordinator chooses a consistent mode under its room lease.
type BackupRouter struct {
	BackupExecutor
	Distributed DistributedBackupCreator
}

func (r BackupRouter) Create(ctx context.Context, roomID, name string, kind backups.Kind, jobID string) (backups.Backup, error) {
	value, err := r.BackupExecutor.Create(ctx, roomID, name, kind, jobID)
	if !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) {
		return value, err
	}
	created, err := r.Distributed.CreateWithMode(ctx, roomID, name, string(kind), jobID, distributedbackup.ModeAutomatic)
	if err != nil {
		return backups.Backup{}, err
	}
	return backups.Backup{ID: created.ID, RoomID: roomID, Name: created.Name, Kind: kind, Status: string(created.Status), CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt}, nil
}
