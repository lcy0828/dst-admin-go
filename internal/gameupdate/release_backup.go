package gameupdate

import (
	"context"
	"errors"

	"dont/internal/distributedbackup"
	"dont/internal/operationlease"
)

type DistributedReleaseProtection struct {
	backups *distributedbackup.Coordinator
}

func NewDistributedReleaseProtection(backups *distributedbackup.Coordinator) (*DistributedReleaseProtection, error) {
	if backups == nil {
		return nil, ErrReleaseInvalid
	}
	return &DistributedReleaseProtection{backups: backups}, nil
}

func (p *DistributedReleaseProtection) CreateProtection(ctx context.Context, roomID, name, sourceJobID string, lease *operationlease.Lease) (string, error) {
	value, err := p.backups.CreateProtected(ctx, roomID, name, sourceJobID, lease)
	if err != nil {
		return "", err
	}
	if value.ID == "" {
		return "", errors.New("distributed protection backup returned an empty ID")
	}
	return value.ID, nil
}
