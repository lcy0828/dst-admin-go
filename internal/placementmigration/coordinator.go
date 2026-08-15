package placementmigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/operationlease"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"

	"github.com/google/uuid"
)

var (
	ErrRevisionChanged = errors.New("topology revision changed before migration")
	ErrShardRunning    = errors.New("shard must be stopped before migration")
)

type Topology interface {
	PrepareMigration(context.Context, string, string) (topology.MigrationPlacement, error)
	ApplyMigration(string, string, string, string) (topology.ExecutionPlacement, error)
}

type RuntimeRouter interface {
	MigrationTargets(topology.MigrationPlacement) (runtimedriver.Driver, runtimedriver.Target, runtimedriver.Driver, runtimedriver.Target, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type Result struct {
	MigrationID      string    `json:"migrationId"`
	RoomID           string    `json:"roomId"`
	WorldID          string    `json:"worldId"`
	SourceTargetID   string    `json:"sourceTargetId"`
	TargetTargetID   string    `json:"targetTargetId"`
	PreviousRevision string    `json:"previousRevision"`
	AppliedRevision  string    `json:"appliedRevision"`
	RecoveryRef      string    `json:"recoveryRef"`
	BytesTransferred int64     `json:"bytesTransferred"`
	SHA256           string    `json:"sha256"`
	CleanupWarnings  []string  `json:"cleanupWarnings"`
	CompletedAt      time.Time `json:"completedAt"`
}

type Coordinator struct {
	topology Topology
	runtimes RuntimeRouter
	leases   LeaseService
	leaseTTL time.Duration
}

func New(topologyService Topology, runtimes RuntimeRouter, leases LeaseService) (*Coordinator, error) {
	if topologyService == nil || runtimes == nil || leases == nil {
		return nil, errors.New("placement migration dependencies are required")
	}
	return &Coordinator{topology: topologyService, runtimes: runtimes, leases: leases, leaseTTL: 2 * time.Minute}, nil
}

func (c *Coordinator) Migrate(ctx context.Context, roomID, worldID, expectedRevision string) (result Result, returnErr error) {
	plan, err := c.topology.PrepareMigration(ctx, roomID, worldID)
	if err != nil {
		return Result{}, err
	}
	if expectedRevision != "" && plan.Revision != expectedRevision {
		return Result{}, ErrRevisionChanged
	}
	sourceDriver, source, targetDriver, target, err := c.runtimes.MigrationTargets(plan)
	if err != nil {
		return Result{}, err
	}
	lease, err := c.leases.Acquire(ctx, roomID, "placement.migrate:"+worldID, c.leaseTTL)
	if err != nil {
		return Result{}, err
	}
	defer c.leases.Release(lease)
	if status, statusErr := sourceDriver.Status(ctx, source); statusErr != nil {
		return Result{}, statusErr
	} else if status.State != string(shards.RuntimeStopped) || status.SessionExists {
		return Result{}, ErrShardRunning
	}

	migrationID := "migration-" + uuid.NewString()
	result = Result{
		MigrationID: migrationID, RoomID: roomID, WorldID: worldID,
		SourceTargetID: plan.SourceTargetID, TargetTargetID: plan.TargetTargetID, PreviousRevision: plan.Revision,
		CleanupWarnings: []string{},
	}
	operation := func() runtimedriver.Operation {
		expires := lease.ExpiresAt.UTC()
		id := uuid.NewString()
		return runtimedriver.Operation{ID: id, Key: id, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
	}
	renew := func() error {
		if time.Until(lease.ExpiresAt) > 45*time.Second {
			return nil
		}
		renewed, renewErr := c.leases.Renew(ctx, lease, c.leaseTTL)
		if renewErr == nil {
			lease = renewed
		}
		return renewErr
	}

	descriptor, err := sourceDriver.PrepareMigrationExport(ctx, source, operation(), migrationID)
	if err != nil {
		return result, err
	}
	exportPrepared := true
	importBegun, targetCommitted, sourceFinalized, topologyApplied := false, false, false, false
	defer func() {
		var cleanupErrors []error
		if !topologyApplied {
			if renewed, renewErr := c.leases.Renew(context.Background(), lease, c.leaseTTL); renewErr == nil {
				lease = renewed
			} else {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("renew migration cleanup lease: %w", renewErr))
			}
			if sourceFinalized {
				if rollbackErr := sourceDriver.RollbackMigrationSource(context.Background(), source, operation(), migrationID); rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration source: %w", rollbackErr))
				}
			}
			if importBegun || targetCommitted {
				if rollbackErr := targetDriver.RollbackMigrationTarget(context.Background(), target, operation(), migrationID); rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration target: %w", rollbackErr))
				}
			}
		}
		if exportPrepared {
			if releaseErr := sourceDriver.ReleaseMigrationExport(context.Background(), source, operation(), migrationID); releaseErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("release migration export: %w", releaseErr))
			}
		}
		if len(cleanupErrors) > 0 {
			returnErr = errors.Join(append([]error{returnErr}, cleanupErrors...)...)
		}
	}()
	if err := targetDriver.BeginMigrationImport(ctx, target, operation(), descriptor); err != nil {
		return result, err
	}
	importBegun = true
	for offset := int64(0); offset < descriptor.Size; {
		if err := renew(); err != nil {
			return result, err
		}
		chunk, readErr := sourceDriver.ReadMigrationExport(ctx, source, migrationID, offset)
		if readErr != nil {
			return result, readErr
		}
		if chunk.Offset != offset || chunk.NextOffset <= offset || chunk.NextOffset > descriptor.Size || chunk.Size != descriptor.Size || chunk.SHA256 != descriptor.SHA256 {
			return result, fmt.Errorf("migration chunk at %d failed integrity validation", offset)
		}
		next, writeErr := targetDriver.WriteMigrationImport(ctx, target, operation(), descriptor, offset, chunk.Data)
		if writeErr != nil {
			return result, writeErr
		}
		if next != chunk.NextOffset {
			return result, fmt.Errorf("migration target acknowledged unexpected offset %d", next)
		}
		offset = next
		result.BytesTransferred = offset
	}
	if err := renew(); err != nil {
		return result, err
	}
	if err := targetDriver.CommitMigrationImport(ctx, target, operation(), migrationID); err != nil {
		return result, err
	}
	targetCommitted = true
	recoveryRef, err := sourceDriver.FinalizeMigrationSource(ctx, source, operation(), migrationID)
	if err != nil {
		return result, err
	}
	sourceFinalized = true
	applied, err := c.topology.ApplyMigration(roomID, worldID, plan.Revision, plan.TargetTargetID)
	if err != nil {
		return result, err
	}
	topologyApplied = true
	result.AppliedRevision, result.RecoveryRef, result.SHA256 = applied.Revision, recoveryRef, descriptor.SHA256
	if err := targetDriver.CompleteMigrationTarget(ctx, target, operation(), migrationID); err != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "目标迁移标记清理失败: "+err.Error())
	}
	if retained, err := sourceDriver.CompleteMigrationSource(ctx, source, operation(), migrationID); err != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "源恢复记录清理失败: "+err.Error())
	} else if retained != "" {
		result.RecoveryRef = retained
	}
	if err := sourceDriver.ReleaseMigrationExport(ctx, source, operation(), migrationID); err != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "源迁移归档清理失败: "+err.Error())
	} else {
		exportPrepared = false
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}
