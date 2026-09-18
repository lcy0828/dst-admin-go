package configuration

import (
	"context"
	"errors"

	"dont/internal/backups"
	"dont/internal/distributedbackup"
	"dont/internal/runtimeguard"
)

type distributedBackupCreator interface {
	CreateWithMode(context.Context, string, string, string, string, string) (distributedbackup.Set, error)
}

// HybridBackupCreator uses the legacy path only for stopped local rooms.
type HybridBackupCreator struct {
	local       BackupCreator
	distributed distributedBackupCreator
}

func NewHybridBackupCreator(local BackupCreator, distributed distributedBackupCreator) (*HybridBackupCreator, error) {
	if local == nil || distributed == nil {
		return nil, errors.New("configuration backup dependencies are required")
	}
	return &HybridBackupCreator{local: local, distributed: distributed}, nil
}

func (c *HybridBackupCreator) Create(ctx context.Context, roomID, name string, kind backups.Kind, sourceJobID string) (backups.Backup, error) {
	value, err := c.local.Create(ctx, roomID, name, kind, sourceJobID)
	if err == nil || !errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable) && !errors.Is(err, backups.ErrConsistentBackupRequired) {
		return value, err
	}
	created, err := c.distributed.CreateWithMode(ctx, roomID, name, string(kind), sourceJobID, distributedbackup.ModeAutomatic)
	if err != nil {
		return backups.Backup{}, err
	}
	return backups.Backup{ID: created.ID, RoomID: roomID, Name: created.Name, Kind: kind, Status: string(created.Status), CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt}, nil
}

var _ BackupCreator = (*HybridBackupCreator)(nil)
