package modcontrol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"dont/internal/distributedbackup"
	"dont/internal/modpublication"
	"dont/internal/operationlease"
)

type LeaseAdapter struct {
	service *operationlease.Service
}

func NewLeaseAdapter(service *operationlease.Service) (*LeaseAdapter, error) {
	if service == nil {
		return nil, ErrInvalidRequest
	}
	return &LeaseAdapter{service: service}, nil
}

func (a *LeaseAdapter) Acquire(ctx context.Context, resourceID, operationKey string, ttl time.Duration) (modpublication.Fence, error) {
	value, err := a.service.Acquire(ctx, resourceID, operationKey, ttl)
	return publicationFence(value), err
}

func (a *LeaseAdapter) Renew(ctx context.Context, fence modpublication.Fence, ttl time.Duration) (modpublication.Fence, error) {
	value, err := a.service.Renew(ctx, operationLease(fence), ttl)
	return publicationFence(value), err
}

func (a *LeaseAdapter) Release(fence modpublication.Fence) error {
	return a.service.Release(operationLease(fence))
}

type BackupAdapter struct {
	coordinator *distributedbackup.Coordinator
}

func NewBackupAdapter(coordinator *distributedbackup.Coordinator) (*BackupAdapter, error) {
	if coordinator == nil {
		return nil, ErrInvalidRequest
	}
	return &BackupAdapter{coordinator: coordinator}, nil
}

func (a *BackupAdapter) CreateProtection(ctx context.Context, request modpublication.ProtectionRequest) (modpublication.ProtectionBackup, error) {
	roomIDs := append([]string(nil), request.RoomIDs...)
	sort.Strings(roomIDs)
	result := modpublication.ProtectionBackup{}
	for _, roomID := range roomIDs {
		var borrowed *operationlease.Lease
		for _, fence := range request.Fences {
			if fence.RoomID == roomID {
				value := operationLease(fence)
				borrowed = &value
				break
			}
		}
		if borrowed == nil {
			return result, fmt.Errorf("%w: room %s protection fence missing", modpublication.ErrInvalidInput, roomID)
		}
		created, err := a.coordinator.CreateProtected(ctx, roomID, "Mod 发布前保护备份", request.PublicationID, borrowed)
		if err != nil {
			return result, err
		}
		if created.ID == "" {
			return result, errors.New("distributed protection backup returned an empty ID")
		}
		result.IDs = append(result.IDs, created.ID)
	}
	return result, nil
}

func publicationFence(value operationlease.Lease) modpublication.Fence {
	return modpublication.Fence{
		RoomID: value.RoomID, LeaseID: value.LeaseID, OperationKey: value.OperationKey,
		FencingToken: value.FencingToken, ExpiresAt: value.ExpiresAt,
	}
}

func operationLease(value modpublication.Fence) operationlease.Lease {
	return operationlease.Lease{
		RoomID: value.RoomID, LeaseID: value.LeaseID, OperationKey: value.OperationKey,
		FencingToken: value.FencingToken, ExpiresAt: value.ExpiresAt,
	}
}
