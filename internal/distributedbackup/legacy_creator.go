package distributedbackup

import (
	"context"

	"dont/internal/backups"
	"dont/internal/runtimeguard"
)

// LegacyCreator preserves the result contract used by existing operations while
// creating all new backups with the consistency coordinator.
type LegacyCreator struct {
	Coordinator *Coordinator
	// Local-only callers must retain their placement guard: a distributed backup
	// must not make a subsequent local filesystem operation appear safe remotely.
	Guard runtimeguard.MutationGuard
}

func (c LegacyCreator) Create(ctx context.Context, roomID, name string, kind backups.Kind, sourceJobID string) (backups.Backup, error) {
	if c.Guard != nil {
		if err := c.Guard.RequireRoom(roomID); err != nil {
			return backups.Backup{}, err
		}
	}
	value, err := c.Coordinator.CreateWithMode(ctx, roomID, name, string(kind), sourceJobID, ModeAutomatic)
	if err != nil {
		return backups.Backup{}, err
	}
	return backups.Backup{
		ID: value.ID, RoomID: value.RoomID, Name: value.Name, Kind: kind,
		Status: string(value.Status), Size: value.Size, ContentSize: value.ContentSize,
		FileCount: value.FileCount, SourceJobID: value.SourceJobID,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, VerifiedAt: value.VerifiedAt,
	}, nil
}
