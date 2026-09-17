package placementmigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/modpublication"
	"dont/internal/operationlease"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

var (
	ErrRevisionChanged = errors.New("topology revision changed before migration")
	ErrShardRunning    = errors.New("shard must be stopped before migration")
	ErrRuntimeRestore  = errors.New("migration completed but runtime state could not be restored")
)

const (
	TransferSourcePeer            = "peer"
	TransferSourceControllerRelay = "controller_relay"
)

type Topology interface {
	PrepareMigration(context.Context, string, string) (topology.MigrationPlacement, error)
	ResolveDesiredShardLinks(context.Context, string) ([]topology.ShardLink, error)
	VerifyDesiredShardLinks(context.Context, string) error
	ApplyMigration(string, string, string, string, string) (topology.ExecutionPlacement, error)
	RollbackMigration(string, string, string, string, string, string, string, []topology.ShardLink) (topology.ExecutionPlacement, error)
}

type RuntimeRouter interface {
	MigrationTargets(topology.MigrationPlacement) (runtimedriver.Driver, runtimedriver.Target, runtimedriver.Driver, runtimedriver.Target, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type ModReconciler interface {
	PreparePlacementMigration(context.Context, topology.MigrationPlacement, operationlease.Lease) (modpublication.PreparedTransaction, error)
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
	TransferSource   string    `json:"transferSource"`
	SHA256           string    `json:"sha256"`
	CleanupWarnings  []string  `json:"cleanupWarnings"`
	WasRunning       bool      `json:"wasRunning"`
	RuntimeRestored  bool      `json:"runtimeRestored"`
	CompletedAt      time.Time `json:"completedAt"`
}

type Coordinator struct {
	topology  Topology
	runtimes  RuntimeRouter
	leases    LeaseService
	leaseTTL  time.Duration
	mutations runtimedriver.RuntimeMutationObserver
	mods      ModReconciler
}

func New(topologyService Topology, runtimes RuntimeRouter, leases LeaseService) (*Coordinator, error) {
	if topologyService == nil || runtimes == nil || leases == nil {
		return nil, errors.New("placement migration dependencies are required")
	}
	return &Coordinator{topology: topologyService, runtimes: runtimes, leases: leases, leaseTTL: 2 * time.Minute}, nil
}

func (c *Coordinator) ConfigureMutationObserver(observer runtimedriver.RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("placement migration mutation observer is required")
	}
	c.mutations = observer
	return nil
}

func (c *Coordinator) ConfigureModReconciler(reconciler ModReconciler) error {
	if reconciler == nil {
		return errors.New("placement migration Mod reconciler is required")
	}
	c.mods = reconciler
	return nil
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
	links, err := c.topology.ResolveDesiredShardLinks(ctx, roomID)
	if err != nil {
		return Result{}, err
	}
	for _, link := range links {
		if link.SourceTargetID == plan.TargetTargetID && link.SourceInstallationID == plan.TargetInstallationID &&
			!runtimedriver.HasTargetCapability(targetDriver, target, runtimedriver.CapabilityShardRouting) {
			return Result{}, fmt.Errorf("目标运行节点不支持迁移时写入跨机器 Shard 互联线路，请先升级对应 Agent: %w", runtimedriver.ErrCapabilityMissing)
		}
	}
	if len(links) > 0 {
		if err := c.topology.VerifyDesiredShardLinks(ctx, roomID); err != nil {
			return Result{}, fmt.Errorf("验证跨机器 Shard 互联线路: %w", err)
		}
	}
	defer runtimedriver.NotifyRuntimeTargetsChanged(c.mutations, source.TargetID, target.TargetID)
	lease, err := c.leases.Acquire(ctx, roomID, "placement.migrate:"+worldID, c.leaseTTL)
	if err != nil {
		return Result{}, err
	}
	defer c.leases.Release(lease)

	migrationID := "migration-" + uuid.NewString()
	result = Result{
		MigrationID: migrationID, RoomID: roomID, WorldID: worldID,
		SourceTargetID: plan.SourceTargetID, TargetTargetID: plan.TargetTargetID, PreviousRevision: plan.Revision,
		TransferSource: TransferSourceControllerRelay, CleanupWarnings: []string{},
	}
	operation := func() runtimedriver.Operation {
		expires := lease.ExpiresAt.UTC()
		id := uuid.NewString()
		return runtimedriver.Operation{ID: id, Key: id, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
	}
	var modTransaction modpublication.PreparedTransaction
	if c.mods != nil {
		modTransaction, err = c.mods.PreparePlacementMigration(ctx, plan, lease)
		if err != nil {
			return result, fmt.Errorf("在目标位置预取并暂存 Mod: %w", err)
		}
		defer modTransaction.Close()
	}

	sourceMayBeStopped, sourceRestored, migrationCommitted := false, false, false
	defer func() {
		if migrationCommitted || !result.WasRunning || !sourceMayBeStopped || sourceRestored {
			return
		}
		if renewed, renewErr := c.leases.Renew(context.Background(), lease, c.leaseTTL); renewErr == nil {
			lease = renewed
		} else {
			returnErr = errors.Join(returnErr, fmt.Errorf("续期源分片恢复租约: %w", renewErr))
			return
		}
		restoreContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, restartErr := sourceDriver.ExecuteShard(restoreContext, source, operation(), shared.ShardActionStart, 2*time.Minute); restartErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("恢复源分片运行状态: %w", restartErr))
		} else {
			result.RuntimeRestored = true
			sourceRestored = true
		}
	}()
	if status, statusErr := sourceDriver.Status(ctx, source); statusErr != nil {
		return Result{}, statusErr
	} else if status.State != string(shards.RuntimeStopped) || status.SessionExists {
		result.WasRunning = status.State == string(shards.RuntimeRunning) || status.State == string(shards.RuntimeStarting)
		sourceMayBeStopped = true
		if _, stopErr := sourceDriver.ExecuteShard(ctx, source, operation(), shared.ShardActionStop, 2*time.Minute); stopErr != nil {
			return result, fmt.Errorf("停止迁移源分片: %w", stopErr)
		}
	}

	renew := func() error {
		if time.Until(lease.ExpiresAt) > 45*time.Second {
			return nil
		}
		renewed, renewErr := c.leases.Renew(ctx, lease, c.leaseTTL)
		if renewErr == nil {
			lease = renewed
		}
		if renewErr != nil {
			return renewErr
		}
		if modTransaction != nil {
			return modTransaction.Renew(ctx)
		}
		return nil
	}

	descriptor, err := sourceDriver.PrepareMigrationExport(ctx, source, operation(), migrationID)
	if err != nil {
		return result, err
	}
	descriptor.ShardBindAll = len(links) > 0
	for _, link := range links {
		if link.SourceTargetID == plan.TargetTargetID && link.SourceInstallationID == plan.TargetInstallationID {
			descriptor.ShardMasterAddress, descriptor.ShardMasterPort = link.Address, link.Port
			break
		}
	}
	exportPrepared := true
	importBegun, targetCommitted, sourceFinalized := false, false, false
	topologyApplied, targetStarted := false, false
	appliedRevision := ""
	defer func() {
		var cleanupErrors []error
		if !migrationCommitted {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if renewed, renewErr := c.leases.Renew(context.Background(), lease, c.leaseTTL); renewErr == nil {
				lease = renewed
			} else {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("renew migration cleanup lease: %w", renewErr))
			}
			if targetStarted {
				result.RuntimeRestored = false
				if _, stopErr := targetDriver.ExecuteShard(cleanupContext, target, operation(), shared.ShardActionStop, 2*time.Minute); stopErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("stop migration target during rollback: %w", stopErr))
				}
			}
			if topologyApplied {
				rolledBack, rollbackErr := c.topology.RollbackMigration(
					roomID, worldID, appliedRevision, plan.SourceTargetID, plan.SourceInstallationID,
					plan.TargetTargetID, plan.TargetInstallationID, plan.AppliedShardLinks,
				)
				if rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration topology: %w", rollbackErr))
				} else {
					source.TopologyRevision = rolledBack.Revision
				}
			}
			if modTransaction != nil {
				if _, rollbackErr := modTransaction.Rollback(cleanupContext, "MIGRATION_ROLLED_BACK", errors.New("placement migration did not commit")); rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration Mod publication: %w", rollbackErr))
				}
			}
			if sourceFinalized {
				if rollbackErr := sourceDriver.RollbackMigrationSource(cleanupContext, source, operation(), migrationID); rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration source: %w", rollbackErr))
				}
			}
			if importBegun || targetCommitted {
				if rollbackErr := targetDriver.RollbackMigrationTarget(cleanupContext, target, operation(), migrationID); rollbackErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("rollback migration target: %w", rollbackErr))
				}
			}
			if result.WasRunning && sourceMayBeStopped {
				if _, restartErr := sourceDriver.ExecuteShard(cleanupContext, source, operation(), shared.ShardActionStart, 2*time.Minute); restartErr != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("restore migration source runtime: %w", restartErr))
				} else {
					result.RuntimeRestored = true
					sourceRestored = true
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
	peerTransferred, peerErr := transferMigrationPeer(ctx, sourceDriver, source, targetDriver, target, descriptor, operation(), renew)
	if peerErr != nil && !errors.Is(peerErr, runtimedriver.ErrMigrationPeerFallback) {
		return result, peerErr
	}
	if peerTransferred && peerErr == nil {
		result.BytesTransferred = descriptor.Size
		result.TransferSource = TransferSourcePeer
	} else {
		if peerErr != nil {
			// A confirmed failed or undispatched Peer fetch may have left a
			// partial staging file. Re-begin with a new idempotency key before
			// entering the compatibility relay.
			if err := targetDriver.BeginMigrationImport(ctx, target, operation(), descriptor); err != nil {
				return result, errors.Join(peerErr, fmt.Errorf("重新初始化迁移导入暂存: %w", err))
			}
		}
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
	}
	if err := renew(); err != nil {
		return result, err
	}
	if err := targetDriver.CommitMigrationImport(ctx, target, operation(), migrationID); err != nil {
		return result, err
	}
	targetCommitted = true
	if modTransaction != nil {
		if _, err := modTransaction.Publish(ctx); err != nil {
			return result, fmt.Errorf("%w: 在目标位置提交 Mod 与世界配置: %v", ErrRuntimeRestore, err)
		}
	}
	applied, err := c.topology.ApplyMigration(roomID, worldID, plan.Revision, plan.TargetTargetID, plan.TargetInstallationID)
	if err != nil {
		return result, err
	}
	topologyApplied = true
	appliedRevision = applied.Revision
	target.TopologyRevision = applied.Revision
	result.AppliedRevision, result.SHA256 = applied.Revision, descriptor.SHA256
	if result.WasRunning {
		if err := renew(); err != nil {
			return result, errors.Join(ErrRuntimeRestore, err)
		}
		startOperation := operation()
		if modTransaction != nil {
			startOperation.LaunchOptions.SkipUpdateServerMods = true
		}
		started, err := targetDriver.ExecuteShard(ctx, target, startOperation, shared.ShardActionStart, 2*time.Minute)
		if err != nil {
			return result, fmt.Errorf("%w: 在新位置启动分片: %v", ErrRuntimeRestore, err)
		}
		targetStarted = true
		if started.Status.State != string(shards.RuntimeRunning) && started.Status.State != string(shards.RuntimeStarting) && !started.Status.SessionExists {
			return result, fmt.Errorf("%w: 新位置启动后未发现运行中的分片", ErrRuntimeRestore)
		}
		result.RuntimeRestored = true
	}
	if modTransaction != nil {
		publication, commitErr := modTransaction.Commit(ctx)
		if commitErr != nil && !publication.CommitDecision {
			return result, fmt.Errorf("提交迁移 Mod 事务: %w", commitErr)
		}
		if commitErr != nil {
			result.CleanupWarnings = append(result.CleanupWarnings, "Mod 发布已经提交，但清理仍需后台恢复: "+commitErr.Error())
		}
	}
	migrationCommitted = true
	recoveryRef, finalizeErr := sourceDriver.FinalizeMigrationSource(ctx, source, operation(), migrationID)
	if finalizeErr != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "源分片恢复副本暂未归档: "+finalizeErr.Error())
	} else {
		sourceFinalized = true
		result.RecoveryRef = recoveryRef
	}
	if err := targetDriver.CompleteMigrationTarget(ctx, target, operation(), migrationID); err != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "目标迁移标记清理失败: "+err.Error())
	}
	if sourceFinalized {
		if retained, err := sourceDriver.CompleteMigrationSource(ctx, source, operation(), migrationID); err != nil {
			result.CleanupWarnings = append(result.CleanupWarnings, "源恢复记录清理失败: "+err.Error())
		} else if retained != "" {
			result.RecoveryRef = retained
		}
	}
	if err := sourceDriver.ReleaseMigrationExport(ctx, source, operation(), migrationID); err != nil {
		result.CleanupWarnings = append(result.CleanupWarnings, "源迁移归档清理失败: "+err.Error())
	} else {
		exportPrepared = false
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func transferMigrationPeer(
	ctx context.Context,
	sourceDriver runtimedriver.Driver,
	source runtimedriver.Target,
	targetDriver runtimedriver.Driver,
	target runtimedriver.Target,
	descriptor runtimedriver.MigrationDescriptor,
	operation runtimedriver.Operation,
	renew func() error,
) (bool, error) {
	sourcePeer, sourceOK := sourceDriver.(runtimedriver.MigrationPeerSource)
	targetPeer, targetOK := targetDriver.(runtimedriver.MigrationPeerTarget)
	if !sourceOK || !targetOK ||
		!runtimedriver.HasTargetCapability(sourceDriver, source, runtimedriver.CapabilityMigrationPeer) ||
		!runtimedriver.HasTargetCapability(targetDriver, target, runtimedriver.CapabilityMigrationPeer) {
		return false, nil
	}
	location, err := sourcePeer.GrantMigrationExport(ctx, source, descriptor, target.TargetID)
	if err != nil {
		return false, errors.Join(runtimedriver.ErrMigrationPeerFallback, err)
	}
	type fetchResult struct {
		bytes int64
		err   error
	}
	peerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan fetchResult, 1)
	go func() {
		bytes, fetchErr := targetPeer.FetchMigrationImport(peerContext, target, operation, descriptor, []shared.RuntimeMigrationFetchLocation{location})
		completed <- fetchResult{bytes: bytes, err: fetchErr}
	}()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case fetched := <-completed:
			if fetched.err != nil {
				return true, fetched.err
			}
			if fetched.bytes != descriptor.Size {
				return true, errors.Join(runtimedriver.ErrMigrationPeerFallback, errors.New("迁移 Peer 返回了不完整的制品"))
			}
			return true, nil
		case <-ticker.C:
			if err := renew(); err != nil {
				cancel()
				return true, fmt.Errorf("续期迁移 Peer 传输租约: %w", err)
			}
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}
