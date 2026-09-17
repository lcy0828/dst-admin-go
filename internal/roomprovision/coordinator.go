package roomprovision

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/topology"
	"dont/shared"

	"github.com/go-ini/ini"
	"github.com/google/uuid"
)

const provisionLeaseTTL = 5 * time.Minute

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	ProvisionBundle(string) (rooms.ProvisionBundle, error)
	StageProvisionCluster(string, string, []byte) (bool, error)
	PublishProvisionCluster(string, string) error
	RollbackProvisionCluster(string, string) error
	CompleteProvisionCluster(string, string) error
}

type Topology interface {
	ResolveDesiredRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
	ResolveDesiredShardLinks(context.Context, string) ([]topology.ShardLink, error)
	VerifyDesiredShardLinks(context.Context, string) error
	ApplyProvision(string, string, []topology.PlacementInput) (string, error)
	Infrastructure(context.Context) (topology.InfrastructureSnapshot, error)
}

type RuntimeRouter interface {
	DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error)
	ProvisionTarget(topology.ExecutionPlacement) (runtimedriver.Driver, runtimedriver.Target, error)
	TrustedTarget(runtimedriver.Target) (runtimedriver.Driver, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type Coordinator struct {
	rooms    RoomCatalog
	topology Topology
	runtimes RuntimeRouter
	leases   LeaseService
	store    *Store
	now      func() time.Time
}

type provisionPlan struct {
	placement  topology.ExecutionPlacement
	driver     runtimedriver.Driver
	target     runtimedriver.Target
	archive    []byte
	descriptor runtimedriver.MigrationDescriptor
}

func NewCoordinator(roomCatalog RoomCatalog, topologyService Topology, runtimes RuntimeRouter, leases LeaseService, store *Store) (*Coordinator, error) {
	if roomCatalog == nil || topologyService == nil || runtimes == nil || leases == nil || store == nil {
		return nil, errors.New("room provision dependencies are required")
	}
	return &Coordinator{rooms: roomCatalog, topology: topologyService, runtimes: runtimes, leases: leases, store: store, now: time.Now}, nil
}

func (c *Coordinator) List(roomID string) ([]Operation, error) { return c.store.List(roomID) }
func (c *Coordinator) Get(id string) (Operation, error)        { return c.store.Get(id) }

func (c *Coordinator) Provision(ctx context.Context, roomID, expectedRevision, sourceJobID string) (Operation, error) {
	ctx, releaseRoom, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return Operation{}, err
	}
	defer releaseRoom()
	lease, err := c.leases.Acquire(ctx, roomID, "room.provision:"+uuid.NewString(), provisionLeaseTTL)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = c.leases.Release(lease) }()
	return c.provision(ctx, roomID, expectedRevision, sourceJobID, &lease)
}

func (c *Coordinator) provision(ctx context.Context, roomID, expectedRevision, sourceJobID string, lease *operationlease.Lease) (Operation, error) {
	bundle, err := c.rooms.ProvisionBundle(roomID)
	if err != nil {
		return Operation{}, err
	}
	placements, err := c.topology.ResolveDesiredRoomExecutions(ctx, roomID)
	if err != nil || len(placements) != len(bundle.Worlds) || len(placements) == 0 {
		return Operation{}, errors.Join(ErrInvalidInput, err)
	}
	revision := placements[0].Revision
	if expectedRevision == "" || expectedRevision != revision {
		return Operation{}, ErrTopologyChanged
	}
	for _, placement := range placements {
		if placement.Revision != revision {
			return Operation{}, ErrTopologyChanged
		}
	}
	links, err := c.topology.ResolveDesiredShardLinks(ctx, roomID)
	if err != nil {
		return Operation{}, err
	}
	sharedByEndpoint, localShared, localClusterChange, err := c.renderSharedConfigurations(ctx, bundle.Shared, placements, links)
	if err != nil {
		return Operation{}, err
	}
	worlds := make(map[string]rooms.ProvisionWorld, len(bundle.Worlds))
	for _, world := range bundle.Worlds {
		worlds[world.World.ID] = world
	}
	plans := make([]provisionPlan, 0, len(placements))
	inputs := make([]topology.PlacementInput, 0, len(placements))
	steps := make([]Step, 0, len(placements))
	operationID := uuid.NewString()
	now := c.now().UTC()
	for index, placement := range placements {
		world, ok := worlds[placement.World.ID]
		if !ok {
			return Operation{}, ErrInvalidInput
		}
		inputs = append(inputs, topology.PlacementInput{
			WorldID: placement.World.ID, TargetID: placement.DesiredTargetID, InstallationID: placement.DesiredInstallationID,
		})
		appliedDriver, appliedTarget, resolveErr := c.runtimes.DriverTarget(ctx, roomID, placement.World.ID)
		if resolveErr != nil {
			return Operation{}, resolveErr
		}
		status, statusErr := appliedDriver.Status(ctx, appliedTarget)
		if statusErr != nil {
			return Operation{}, statusErr
		}
		driver, target, resolveErr := c.runtimes.ProvisionTarget(placement)
		if resolveErr != nil {
			return Operation{}, resolveErr
		}
		step := Step{
			ID: fmt.Sprintf("%s-%02d", operationID, index), OperationID: operationID,
			WorldID: world.World.ID, WorldName: world.World.Name,
			SourceTargetID: appliedTarget.TargetID, SourceInstallationID: appliedTarget.InstallationID,
			TargetID:       target.TargetID,
			InstallationID: target.InstallationID, Cluster: target.Cluster, Shard: target.Shard,
			Phase:      "not_started",
			WasRunning: status.State == string(shards.RuntimeRunning) || status.State == string(shards.RuntimeStarting),
			UpdatedAt:  now,
		}
		if placement.DesiredTargetID == placement.AppliedTargetID &&
			placement.DesiredInstallationID == placement.AppliedInstallationID {
			step.Phase = "existing"
			steps = append(steps, step)
			continue
		}
		if placement.AppliedTargetID != "local" {
			return Operation{}, ErrMigrationNeeded
		}
		if placement.DesiredTargetID == "local" || inventoryContainsShard(placement, target.Cluster, target.Shard) {
			return Operation{}, ErrTargetExists
		}
		shared := sharedByEndpoint[provisionEndpointKey(placement.DesiredTargetID, placement.DesiredInstallationID)]
		archive, descriptor, archiveErr := buildProvisionArchive(operationID, index, shared, world.Files)
		if archiveErr != nil {
			return Operation{}, archiveErr
		}
		step.MigrationID, step.Size, step.SHA256 = descriptor.MigrationID, descriptor.Size, descriptor.SHA256
		steps = append(steps, step)
		plans = append(plans, provisionPlan{placement: placement, driver: driver, target: target, archive: archive, descriptor: descriptor})
	}
	if len(plans) == 0 && !localClusterChange {
		return Operation{}, ErrInvalidInput
	}
	operation := Operation{
		ID: operationID, RoomID: roomID, RoomName: bundle.Room.Name, TopologyRevision: revision,
		LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Phase: "planned", Status: StatusRunning,
		SourceJobID: sourceJobID, CreatedAt: now, UpdatedAt: now,
	}
	operation, err = c.store.Create(operation, steps)
	if err != nil {
		return Operation{}, err
	}
	if err := c.stopSources(ctx, &operation, lease); err != nil {
		return c.failBeforeDecision(ctx, operation, lease, err, false)
	}
	if err := c.topology.VerifyDesiredShardLinks(ctx, roomID); err != nil {
		return c.failBeforeDecision(ctx, operation, lease, err, false)
	}
	localStaged := false
	if localClusterChange {
		cluster := provisionFile(localShared, "cluster.ini")
		localStaged, err = c.rooms.StageProvisionCluster(roomID, operation.ID, cluster)
		if err != nil {
			return c.failBeforeDecision(ctx, operation, lease, err, false)
		}
	}
	for index := range plans {
		if err := c.savePhase(&operation, "uploading", StatusRunning, ""); err != nil {
			return operation, err
		}
		if err := c.transferPlan(ctx, &operation, lease, &plans[index]); err != nil {
			return c.failBeforeDecision(ctx, operation, lease, err, localStaged)
		}
		if err := c.saveStepPhase(&operation, plans[index].placement.World.ID, "published", ""); err != nil {
			return c.failBeforeDecision(ctx, operation, lease, err, localStaged)
		}
	}
	if err := c.revalidate(ctx, revision, placements); err != nil {
		return c.failBeforeDecision(ctx, operation, lease, err, localStaged)
	}
	if localStaged {
		if err := c.rooms.PublishProvisionCluster(roomID, operation.ID); err != nil {
			return c.failBeforeDecision(ctx, operation, lease, err, true)
		}
	}
	if err := c.savePhase(&operation, "commit_decided", StatusRunning, ""); err != nil {
		return c.failBeforeDecision(ctx, operation, lease, err, localStaged)
	}
	appliedRevision, err := c.topology.ApplyProvision(roomID, revision, inputs)
	if err != nil {
		return c.requireRecovery(operation, err)
	}
	operation.AppliedRevision = appliedRevision
	if err := c.savePhase(&operation, "topology_committed", StatusRunning, ""); err != nil {
		return c.requireRecovery(operation, err)
	}
	if err := c.completeTargets(ctx, &operation, lease); err != nil {
		return c.requireRecovery(operation, err)
	}
	if localStaged {
		if err := c.rooms.CompleteProvisionCluster(roomID, operation.ID); err != nil {
			return c.requireRecovery(operation, err)
		}
	}
	if err := c.restoreRuntimeState(ctx, &operation, lease, true); err != nil {
		return c.requireRecovery(operation, err)
	}
	if err := c.savePhase(&operation, "completed", StatusSucceeded, ""); err != nil {
		return operation, err
	}
	return c.store.Get(operation.ID)
}

func (c *Coordinator) transferPlan(ctx context.Context, operation *Operation, lease *operationlease.Lease, plan *provisionPlan) error {
	if err := c.renewLease(ctx, lease); err != nil {
		return err
	}
	if err := c.saveStepPhase(operation, plan.placement.World.ID, "planned", ""); err != nil {
		return err
	}
	if err := plan.driver.BeginMigrationImport(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "begin", plan.placement.World.ID, 0), plan.descriptor); err != nil {
		if errors.Is(err, runtimedriver.ErrOperationNotDispatched) {
			if saveErr := c.saveStepPhase(operation, plan.placement.World.ID, "not_started", err.Error()); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	if err := c.saveStepPhase(operation, plan.placement.World.ID, "uploading", ""); err != nil {
		return err
	}
	for offset := int64(0); offset < plan.descriptor.Size; {
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		end := offset + int64(shared.MaxChunkBytes)
		if end > int64(len(plan.archive)) {
			end = int64(len(plan.archive))
		}
		next, err := plan.driver.WriteMigrationImport(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "write", plan.placement.World.ID, offset), plan.descriptor, offset, plan.archive[offset:end])
		if err != nil || next != end {
			return errors.Join(err, ErrRecoveryNeeded)
		}
		offset = next
	}
	return plan.driver.CommitMigrationImport(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "publish", plan.placement.World.ID, 0), plan.descriptor.MigrationID)
}

func (c *Coordinator) completeTargets(ctx context.Context, operation *Operation, lease *operationlease.Lease) error {
	for index := range operation.Steps {
		step := &operation.Steps[index]
		if step.MigrationID == "" || step.Phase == "completed" {
			continue
		}
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		target := targetFromStep(*operation, *step)
		driver, err := c.runtimes.TrustedTarget(target)
		if err == nil {
			err = driver.CompleteMigrationTarget(ctx, target, c.runtimeOperation(*lease, operation.ID, "complete", step.WorldID, 0), step.MigrationID)
		}
		if err != nil {
			step.Failure = err.Error()
			_, _ = c.store.SaveStep(*step)
			return err
		}
		step.Phase, step.Failure = "completed", ""
		saved, err := c.store.SaveStep(*step)
		if err != nil {
			return err
		}
		*step = saved
	}
	return nil
}

func (c *Coordinator) failBeforeDecision(ctx context.Context, operation Operation, lease *operationlease.Lease, cause error, localStaged bool) (Operation, error) {
	rollbackErr := c.rollbackTargets(ctx, &operation, lease)
	if localStaged {
		rollbackErr = errors.Join(rollbackErr, c.rooms.RollbackProvisionCluster(operation.RoomID, operation.ID))
	}
	rollbackErr = errors.Join(rollbackErr, c.restoreRuntimeState(ctx, &operation, lease, false))
	if rollbackErr != nil {
		operation, _ = c.requireRecovery(operation, errors.Join(cause, rollbackErr))
		return operation, errors.Join(cause, rollbackErr, ErrRecoveryNeeded)
	}
	_ = c.savePhase(&operation, "rolled_back", StatusRolledBack, cause.Error())
	return operation, cause
}

func (c *Coordinator) rollbackTargets(ctx context.Context, operation *Operation, lease *operationlease.Lease) error {
	var result error
	for index := len(operation.Steps) - 1; index >= 0; index-- {
		step := &operation.Steps[index]
		if step.MigrationID == "" || step.Phase == "rolled_back" || step.Phase == "existing" {
			continue
		}
		if step.Phase == "not_started" {
			step.Phase, step.Failure = "rolled_back", ""
			if saved, err := c.store.SaveStep(*step); err != nil {
				result = errors.Join(result, err)
			} else {
				*step = saved
			}
			continue
		}
		if err := c.renewLease(ctx, lease); err != nil {
			result = errors.Join(result, err)
			break
		}
		target := targetFromStep(*operation, *step)
		driver, err := c.runtimes.TrustedTarget(target)
		if err == nil {
			err = driver.RollbackMigrationTarget(ctx, target, c.runtimeOperation(*lease, operation.ID, "rollback", step.WorldID, 0), step.MigrationID)
		}
		if err != nil {
			step.Failure = err.Error()
			result = errors.Join(result, err)
		} else {
			step.Phase, step.Failure = "rolled_back", ""
		}
		_, _ = c.store.SaveStep(*step)
	}
	return result
}

func (c *Coordinator) RecoverOperation(ctx context.Context, operationID string) (Operation, error) {
	operation, err := c.store.Get(operationID)
	if err != nil || operation.Status == StatusSucceeded || operation.Status == StatusRolledBack {
		return operation, err
	}
	ctx, releaseRoom, err := roomops.Acquire(ctx, operation.RoomID)
	if err != nil {
		return operation, err
	}
	defer releaseRoom()
	lease, err := c.leases.Acquire(ctx, operation.RoomID, "room.provision.recover:"+operation.ID, provisionLeaseTTL)
	if err != nil {
		return operation, err
	}
	defer func() { _ = c.leases.Release(lease) }()
	operation.LeaseID, operation.FencingToken = lease.LeaseID, lease.FencingToken
	if operation.Phase != "commit_decided" && operation.Phase != "topology_committed" && operation.Phase != "runtime_restore_target" {
		rollbackErr := c.rollbackTargets(ctx, &operation, &lease)
		rollbackErr = errors.Join(rollbackErr, c.rooms.RollbackProvisionCluster(operation.RoomID, operation.ID))
		rollbackErr = errors.Join(rollbackErr, c.restoreRuntimeState(ctx, &operation, &lease, false))
		if rollbackErr != nil {
			return c.requireRecovery(operation, rollbackErr)
		}
		_ = c.savePhase(&operation, "rolled_back", StatusRolledBack, operation.Failure)
		return c.store.Get(operation.ID)
	}
	placements, resolveErr := c.topology.ResolveDesiredRoomExecutions(ctx, operation.RoomID)
	if resolveErr != nil {
		return c.requireRecovery(operation, resolveErr)
	}
	expectedTargets := make(map[string]topology.PlacementInput, len(operation.Steps))
	for _, step := range operation.Steps {
		if step.WorldID == "" || step.TargetID == "" {
			return c.requireRecovery(operation, ErrTopologyChanged)
		}
		if _, exists := expectedTargets[step.WorldID]; exists {
			return c.requireRecovery(operation, ErrTopologyChanged)
		}
		expectedTargets[step.WorldID] = topology.PlacementInput{
			WorldID: step.WorldID, TargetID: step.TargetID, InstallationID: step.InstallationID,
		}
	}
	if len(placements) != len(expectedTargets) {
		return c.requireRecovery(operation, ErrTopologyChanged)
	}
	allApplied := true
	inputs := make([]topology.PlacementInput, 0, len(placements))
	for _, placement := range placements {
		target, ok := expectedTargets[placement.World.ID]
		desiredInstallationID := placement.DesiredInstallationID
		if desiredInstallationID == "" {
			desiredInstallationID = target.InstallationID
		}
		appliedInstallationID := placement.AppliedInstallationID
		if appliedInstallationID == "" && placement.AppliedTargetID == target.TargetID {
			appliedInstallationID = target.InstallationID
		}
		if !ok || placement.DesiredTargetID != target.TargetID || desiredInstallationID != target.InstallationID {
			return c.requireRecovery(operation, ErrTopologyChanged)
		}
		inputs = append(inputs, target)
		allApplied = allApplied && placement.AppliedTargetID == target.TargetID && appliedInstallationID == target.InstallationID
	}
	if !allApplied {
		for _, placement := range placements {
			if placement.Revision != operation.TopologyRevision {
				return c.requireRecovery(operation, ErrTopologyChanged)
			}
		}
		applied, applyErr := c.topology.ApplyProvision(operation.RoomID, operation.TopologyRevision, inputs)
		if applyErr != nil {
			return c.requireRecovery(operation, applyErr)
		}
		operation.AppliedRevision = applied
	}
	_ = c.savePhase(&operation, "topology_committed", StatusRunning, "")
	if err := c.completeTargets(ctx, &operation, &lease); err != nil {
		return c.requireRecovery(operation, err)
	}
	_ = c.rooms.CompleteProvisionCluster(operation.RoomID, operation.ID)
	if err := c.restoreRuntimeState(ctx, &operation, &lease, true); err != nil {
		return c.requireRecovery(operation, err)
	}
	if err := c.savePhase(&operation, "completed", StatusSucceeded, ""); err != nil {
		return operation, err
	}
	return c.store.Get(operation.ID)
}

func (c *Coordinator) RecoverPending(ctx context.Context) error {
	values, err := c.store.Active()
	if err != nil {
		return err
	}
	var result error
	for _, value := range values {
		_, recoverErr := c.RecoverOperation(ctx, value.ID)
		result = errors.Join(result, recoverErr)
	}
	return result
}

func (c *Coordinator) renderSharedConfigurations(ctx context.Context, files []rooms.ProvisionFile, placements []topology.ExecutionPlacement, links []topology.ShardLink) (map[string][]rooms.ProvisionFile, []rooms.ProvisionFile, bool, error) {
	targets := make(map[string]bool)
	endpoints := make(map[string]topology.ExecutionPlacement)
	masterTarget, masterInstallation := "", ""
	for _, placement := range placements {
		targets[placement.DesiredTargetID] = true
		endpoints[provisionEndpointKey(placement.DesiredTargetID, placement.DesiredInstallationID)] = placement
		if placement.World.Role == rooms.WorldRoleMaster || placement.World.IsMaster {
			if masterTarget != "" {
				return nil, nil, false, ErrInvalidInput
			}
			masterTarget, masterInstallation = placement.DesiredTargetID, placement.DesiredInstallationID
		}
	}
	result := make(map[string][]rooms.ProvisionFile, len(endpoints))
	if len(targets) < 2 {
		for key := range endpoints {
			result[key] = cloneProvisionFiles(files)
		}
		return result, nil, false, nil
	}
	if masterTarget == "" {
		return nil, nil, false, ErrInvalidInput
	}
	linkByEndpoint := make(map[string]topology.ShardLink, len(links))
	for _, link := range links {
		if link.MasterTargetID != masterTarget || link.MasterInstallationID != masterInstallation {
			continue
		}
		linkByEndpoint[provisionEndpointKey(link.SourceTargetID, link.SourceInstallationID)] = link
	}
	legacyMasterIP := ""
	for key, placement := range endpoints {
		if placement.DesiredTargetID != masterTarget {
			if _, ok := linkByEndpoint[key]; !ok {
				infrastructure, err := c.topology.Infrastructure(ctx)
				if err != nil {
					return nil, nil, false, err
				}
				profile, exists := profilesByTarget(infrastructure)[masterTarget]
				legacyMasterIP = strings.TrimSpace(profile.AdvertiseAddress)
				if !exists || !routableAddress(legacyMasterIP) {
					return nil, nil, false, fmt.Errorf("%w: 必须为 Secondary 选择可达的 Master 互联地址", ErrTargetNotReady)
				}
				break
			}
		}
	}
	originalCluster := provisionFile(files, "cluster.ini")
	var localShared []rooms.ProvisionFile
	localChanged := false
	for key, placement := range endpoints {
		rendered := cloneProvisionFiles(files)
		cluster := provisionFile(rendered, "cluster.ini")
		config, err := ini.Load(cluster)
		if err != nil {
			return nil, nil, false, err
		}
		section := config.Section("SHARD")
		section.Key("bind_ip").SetValue("0.0.0.0")
		if placement.DesiredTargetID != masterTarget {
			address, port := legacyMasterIP, section.Key("master_port").MustInt(0)
			if link, exists := linkByEndpoint[key]; exists {
				address, port = strings.TrimSpace(link.Address), link.Port
			}
			if !routableAddress(address) || port < 1 || port > 65535 {
				return nil, nil, false, fmt.Errorf("%w: Secondary 的 Master 互联端点无效", ErrTargetNotReady)
			}
			section.Key("master_ip").SetValue(address)
			section.Key("master_port").SetValue(fmt.Sprintf("%d", port))
		}
		var encoded bytes.Buffer
		if _, err := config.WriteTo(&encoded); err != nil {
			return nil, nil, false, err
		}
		for index := range rendered {
			if rendered[index].Name == "cluster.ini" {
				rendered[index].Data = append([]byte(nil), encoded.Bytes()...)
			}
		}
		result[key] = rendered
		if placement.DesiredTargetID == "local" {
			localShared = rendered
			localChanged = !bytes.Equal(originalCluster, encoded.Bytes())
		}
	}
	return result, localShared, localChanged, nil
}

func provisionEndpointKey(targetID, installationID string) string {
	return strings.TrimSpace(targetID) + "\x00" + strings.TrimSpace(installationID)
}

func (c *Coordinator) revalidate(ctx context.Context, revision string, expected []topology.ExecutionPlacement) error {
	current, err := c.topology.ResolveDesiredRoomExecutions(ctx, expected[0].Room.ID)
	if err != nil || len(current) != len(expected) {
		return errors.Join(ErrTopologyChanged, err)
	}
	byWorld := make(map[string]topology.ExecutionPlacement, len(expected))
	for _, placement := range expected {
		byWorld[placement.World.ID] = placement
	}
	for _, placement := range current {
		previous, ok := byWorld[placement.World.ID]
		if !ok || placement.Revision != revision ||
			placement.DesiredTargetID != previous.DesiredTargetID ||
			placement.DesiredInstallationID != previous.DesiredInstallationID {
			return ErrTopologyChanged
		}
	}
	return nil
}

func (c *Coordinator) renewLease(ctx context.Context, lease *operationlease.Lease) error {
	if time.Until(lease.ExpiresAt) > time.Minute {
		return nil
	}
	value, err := c.leases.Renew(ctx, *lease, provisionLeaseTTL)
	if err == nil {
		*lease = value
	}
	return err
}

func (c *Coordinator) stopSources(ctx context.Context, operation *Operation, lease *operationlease.Lease) error {
	if err := c.savePhase(operation, "stopping", StatusRunning, ""); err != nil {
		return err
	}
	for index := range operation.Steps {
		step := &operation.Steps[index]
		target := sourceTargetFromStep(*operation, *step)
		driver, err := c.runtimes.TrustedTarget(target)
		if err != nil {
			return err
		}
		status, err := driver.Status(ctx, target)
		if err != nil {
			return err
		}
		if status.State == string(shards.RuntimeStopped) && !status.SessionExists {
			continue
		}
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		if _, err := driver.ExecuteShard(ctx, target, c.runtimeOperation(*lease, operation.ID, "stop", step.WorldID, 0), shared.ShardActionStop, 2*time.Minute); err != nil {
			return fmt.Errorf("停止世界 %s: %w", step.WorldName, err)
		}
	}
	return c.savePhase(operation, "stopped", StatusRunning, "")
}

func (c *Coordinator) restoreRuntimeState(ctx context.Context, operation *Operation, lease *operationlease.Lease, onTarget bool) error {
	phase := "runtime_restore_source"
	if onTarget {
		phase = "runtime_restore_target"
	}
	if err := c.savePhase(operation, phase, StatusRunning, ""); err != nil {
		return err
	}
	for index := range operation.Steps {
		step := &operation.Steps[index]
		if !step.WasRunning || step.RuntimeRestored {
			continue
		}
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		target := sourceTargetFromStep(*operation, *step)
		if onTarget {
			target = targetFromStep(*operation, *step)
			target.TopologyRevision = operation.AppliedRevision
		}
		driver, err := c.runtimes.TrustedTarget(target)
		if err == nil {
			_, err = driver.ExecuteShard(ctx, target, c.runtimeOperation(*lease, operation.ID, "start", step.WorldID, 0), shared.ShardActionStart, 2*time.Minute)
		}
		if err != nil {
			step.Failure = fmt.Sprintf("恢复世界运行状态: %v", err)
			_, _ = c.store.SaveStep(*step)
			return errors.Join(ErrRecoveryNeeded, err)
		}
		step.RuntimeRestored, step.Failure = true, ""
		saved, err := c.store.SaveStep(*step)
		if err != nil {
			return err
		}
		*step = saved
	}
	return nil
}

func (c *Coordinator) runtimeOperation(lease operationlease.Lease, operationID, phase, worldID string, offset int64) runtimedriver.Operation {
	key := fmt.Sprintf("p.%s.%d.%s.%s.%d", operationID, lease.FencingToken, phase, shortIdentity(worldID), offset)
	expires := lease.ExpiresAt.UTC()
	return runtimedriver.Operation{ID: key, Key: key, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
}

func sourceTargetFromStep(operation Operation, step Step) runtimedriver.Target {
	return runtimedriver.Target{
		TargetID: step.SourceTargetID, InstallationID: step.SourceInstallationID,
		RoomID: operation.RoomID, WorldID: step.WorldID, Cluster: step.Cluster, Shard: step.Shard,
		TopologyRevision: operation.TopologyRevision,
	}
}

func (c *Coordinator) savePhase(operation *Operation, phase string, status Status, failure string) error {
	operation.Phase, operation.Status, operation.Failure = phase, status, failure
	saved, err := c.store.SaveOperation(*operation)
	if err == nil {
		*operation = saved
	}
	return err
}

func (c *Coordinator) saveStepPhase(operation *Operation, worldID, phase, failure string) error {
	for index := range operation.Steps {
		step := operation.Steps[index]
		if step.WorldID == worldID {
			step.Phase, step.Failure = phase, failure
			saved, err := c.store.SaveStep(step)
			if err == nil {
				operation.Steps[index] = saved
			}
			return err
		}
	}
	return ErrNotFound
}

func (c *Coordinator) requireRecovery(operation Operation, cause error) (Operation, error) {
	_ = c.savePhase(&operation, operation.Phase, StatusRecoveryRequired, cause.Error())
	return operation, errors.Join(cause, ErrRecoveryNeeded)
}

func buildProvisionArchive(operationID string, index int, shared, world []rooms.ProvisionFile) ([]byte, runtimedriver.MigrationDescriptor, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	runtimeOutput := &zip.FileHeader{Name: "shard/save/mod_config_data/dst-admin/", Method: zip.Store}
	runtimeOutput.SetMode(os.ModeDir | 0o700)
	if _, err := archive.CreateHeader(runtimeOutput); err != nil {
		_ = archive.Close()
		return nil, runtimedriver.MigrationDescriptor{}, err
	}
	write := func(prefix string, files []rooms.ProvisionFile) error {
		for _, file := range files {
			name := filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(file.Name)))
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(file.Mode.Perm())
			writer, err := archive.CreateHeader(header)
			if err != nil {
				return err
			}
			if _, err := writer.Write(file.Data); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write("shared", shared); err != nil {
		_ = archive.Close()
		return nil, runtimedriver.MigrationDescriptor{}, err
	}
	if err := write("shard", world); err != nil {
		_ = archive.Close()
		return nil, runtimedriver.MigrationDescriptor{}, err
	}
	if err := archive.Close(); err != nil {
		return nil, runtimedriver.MigrationDescriptor{}, err
	}
	data := buffer.Bytes()
	sum := sha256.Sum256(data)
	id := fmt.Sprintf("provision-%s-%02d", strings.ReplaceAll(operationID, "-", ""), index)
	return data, runtimedriver.MigrationDescriptor{MigrationID: id, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func inventoryContainsShard(placement topology.ExecutionPlacement, cluster, shard string) bool {
	for _, room := range placement.Inventory.Inventory.Rooms {
		if !strings.EqualFold(room.Directory, cluster) {
			continue
		}
		for _, item := range room.Shards {
			if strings.EqualFold(item.Directory, shard) {
				return true
			}
		}
	}
	return false
}

func profilesByTarget(value topology.InfrastructureSnapshot) map[string]topology.NetworkProfile {
	profiles := make(map[string]topology.NetworkProfile, len(value.NetworkProfiles))
	byID := make(map[string]topology.NetworkProfile, len(value.NetworkProfiles))
	for _, profile := range value.NetworkProfiles {
		byID[profile.ID] = profile
	}
	for _, environment := range value.Environments {
		if profile, ok := byID[environment.NetworkProfileID]; ok {
			profiles[environment.TargetID] = profile
		}
	}
	return profiles
}

func routableAddress(value string) bool {
	if value == "" || strings.EqualFold(value, "localhost") {
		return false
	}
	ip := net.ParseIP(strings.Trim(value, "[]"))
	if ip == nil {
		return !strings.ContainsAny(value, "\x00\r\n /\\")
	}
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast()
}

func provisionFile(files []rooms.ProvisionFile, name string) []byte {
	for _, file := range files {
		if file.Name == name {
			return file.Data
		}
	}
	return nil
}

func cloneProvisionFiles(values []rooms.ProvisionFile) []rooms.ProvisionFile {
	result := make([]rooms.ProvisionFile, len(values))
	for index, value := range values {
		result[index] = rooms.ProvisionFile{Name: value.Name, Data: append([]byte(nil), value.Data...), Mode: value.Mode}
	}
	return result
}

func targetFromStep(operation Operation, step Step) runtimedriver.Target {
	return runtimedriver.Target{
		TargetID: step.TargetID, InstallationID: step.InstallationID, RoomID: operation.RoomID,
		WorldID: step.WorldID, Cluster: step.Cluster, Shard: step.Shard, TopologyRevision: operation.TopologyRevision,
	}
}

func shortIdentity(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
