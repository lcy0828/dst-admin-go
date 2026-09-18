package distributedbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dont/internal/maintenance"
	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/shards"
	"dont/internal/shardtransfer"
	"dont/internal/tempfiles"
	"dont/internal/topology"
	"dont/shared"

	"github.com/google/uuid"
)

const (
	legacyManifestVersion = 1
	manifestVersion       = 2
	leaseTTL              = 5 * time.Minute
	stopTimeout           = 2 * time.Minute
	ModeCold              = "cold-consistent"
	ModeHot               = "hot-consistent"
	ModeAutomatic         = "automatic"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
}

type PlacementResolver interface {
	ResolveRoomExecutions(context.Context, string) ([]topology.ExecutionPlacement, error)
}

type RuntimeRouter interface {
	DriverTarget(context.Context, string, string) (runtimedriver.Driver, runtimedriver.Target, error)
}

type LeaseService interface {
	Acquire(context.Context, string, string, time.Duration) (operationlease.Lease, error)
	Renew(context.Context, operationlease.Lease, time.Duration) (operationlease.Lease, error)
	Release(operationlease.Lease) error
}

type Coordinator struct {
	root          string
	rooms         RoomCatalog
	placements    PlacementResolver
	runtimes      RuntimeRouter
	leases        LeaseService
	store         *Store
	now           func() time.Time
	mutations     runtimedriver.RuntimeMutationObserver
	deletionGuard func(Set) error
}

type runtimePart struct {
	part   Part
	driver runtimedriver.Driver
	target runtimedriver.Target
	state  string
}

func NewCoordinator(root string, rooms RoomCatalog, placements PlacementResolver, runtimes RuntimeRouter, leases LeaseService, store *Store) (*Coordinator, error) {
	root = strings.TrimSpace(root)
	if root == "" || rooms == nil || placements == nil || runtimes == nil || leases == nil || store == nil {
		return nil, errors.New("distributed backup dependencies are required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	if err := tempfiles.Cleanup(absolute, tempfiles.Exports); err != nil {
		return nil, fmt.Errorf("recover temporary backup exports: %w", err)
	}
	return &Coordinator{root: filepath.Clean(absolute), rooms: rooms, placements: placements, runtimes: runtimes, leases: leases, store: store, now: time.Now}, nil
}

func (c *Coordinator) ConfigureMutationObserver(observer runtimedriver.RuntimeMutationObserver) error {
	if observer == nil {
		return errors.New("distributed backup mutation observer is required")
	}
	c.mutations = observer
	return nil
}

func (c *Coordinator) List(roomID string) ([]Set, error) {
	values, err := c.store.ListSets(roomID)
	if err != nil {
		return nil, err
	}
	for index := range values {
		values[index] = c.classifySet(values[index])
	}
	return values, nil
}

func (c *Coordinator) Get(id string) (Set, error) {
	value, err := c.store.GetSet(id)
	if err != nil {
		return Set{}, err
	}
	return c.classifySet(value), nil
}

func (c *Coordinator) Operations(roomID string) ([]Operation, error) {
	return c.store.ListOperations(roomID)
}

func (c *Coordinator) Operation(id string) (Operation, error) { return c.store.Operation(id) }

func (c *Coordinator) Create(ctx context.Context, roomID, name, kind, sourceJobID string) (Set, error) {
	return c.CreateWithMode(ctx, roomID, name, kind, sourceJobID, ModeCold)
}

func (c *Coordinator) CreateWithMode(ctx context.Context, roomID, name, kind, sourceJobID, mode string) (Set, error) {
	if mode == "" {
		mode = ModeCold
	}
	if mode != ModeCold && mode != ModeHot && mode != ModeAutomatic {
		return Set{}, ErrInvalidInput
	}
	ctx, releaseRoom, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return Set{}, err
	}
	defer releaseRoom()
	operationID := uuid.NewString()
	lease, err := c.leases.Acquire(ctx, roomID, "backup-set.create:"+operationID, leaseTTL)
	if err != nil {
		return Set{}, err
	}
	defer func() { _ = c.leases.Release(lease) }()
	return c.createUsingLease(ctx, roomID, name, kind, sourceJobID, operationID, &lease, mode)
}

// CreateProtected creates a cold-consistent protection backup while borrowing
// a room operation lease owned by the caller. Renewals update lease in place so
// the owner keeps the current expiry. This method never releases the lease.
func (c *Coordinator) CreateProtected(ctx context.Context, roomID, name, sourceJobID string, lease *operationlease.Lease) (Set, error) {
	roomID = strings.TrimSpace(roomID)
	ctx, releaseRoom, err := roomops.Acquire(ctx, roomID)
	if err != nil {
		return Set{}, err
	}
	defer releaseRoom()
	if lease == nil || lease.RoomID != roomID || strings.TrimSpace(lease.LeaseID) == "" ||
		strings.TrimSpace(lease.OperationKey) == "" || lease.FencingToken == 0 {
		return Set{}, ErrInvalidInput
	}
	if err := c.renewLease(ctx, lease); err != nil {
		return Set{}, err
	}
	return c.createUsingLease(ctx, roomID, name, "protection", sourceJobID, uuid.NewString(), lease, ModeCold)
}

func (c *Coordinator) createUsingLease(ctx context.Context, roomID, name, kind, sourceJobID, operationID string, lease *operationlease.Lease, mode string) (Set, error) {
	room, runtimeParts, revision, running, err := c.plan(ctx, roomID)
	if err != nil {
		return Set{}, err
	}
	// Choose under the room lease so the observed running set stays authoritative.
	if mode == ModeAutomatic {
		mode = ModeCold
		if len(runtimeParts) > 0 && len(running) == len(runtimeParts) {
			mode = ModeHot
		}
	}
	set, operation, err := c.initializeSet(room, runtimeParts, revision, running, name, kind, sourceJobID, operationID, *lease, mode)
	if err != nil {
		return Set{}, err
	}
	if mode == ModeHot {
		return c.createHotWithPlan(ctx, set, operation, runtimeParts, lease)
	}
	return c.createWithPlan(ctx, set, operation, runtimeParts, lease, true)
}

func (c *Coordinator) createWithPlan(ctx context.Context, set Set, operation Operation, runtimeParts []runtimePart, lease *operationlease.Lease, restart bool) (result Set, returnErr error) {
	result = set
	originalRunning := append([]string(nil), operation.OriginalRunningWorlds...)
	defer func() {
		if restart {
			if restartErr := c.restartWorlds(context.Background(), runtimeParts, originalRunning, operation, lease); restartErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore original running shards: %w", restartErr))
			}
		}
	}()
	if err := c.saveOperationPhase(&operation, "stopping", OperationRunning, ""); err != nil {
		return result, err
	}
	if err := c.stopAll(ctx, runtimeParts, operation, lease); err != nil {
		return c.failCreate(result, operation, err)
	}
	if err := c.saveOperationPhase(&operation, "staging", OperationRunning, ""); err != nil {
		return c.failCreate(result, operation, err)
	}
	var sharedSHA string
	verified := 0
	for index := range runtimeParts {
		if err := c.renewLease(ctx, lease); err != nil {
			return c.failCreate(result, operation, err)
		}
		current, err := c.stagePart(ctx, runtimeParts[index], operation, lease, index)
		runtimeParts[index].part = current
		if err != nil {
			return c.failCreate(result, operation, err)
		}
		compatibleSHA, compatibilityErr := c.partSharedCompatibilitySHA(current)
		if compatibilityErr != nil {
			return c.failCreate(result, operation, compatibilityErr)
		}
		if sharedSHA == "" {
			sharedSHA = compatibleSHA
		} else if !strings.EqualFold(sharedSHA, compatibleSHA) {
			return c.failCreate(result, operation, ErrSharedFilesDiffer)
		}
		verified++
	}
	if verified != len(runtimeParts) {
		return c.failCreate(result, operation, ErrIncomplete)
	}
	result, err := c.finalizeSet(result.ID, sharedSHA)
	if err != nil {
		return c.failCreate(result, operation, err)
	}
	operation.SetID = result.ID
	if err := c.saveOperationPhase(&operation, "completed", OperationSucceeded, ""); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Coordinator) plan(ctx context.Context, roomID string) (rooms.Room, []runtimePart, string, []string, error) {
	room, err := c.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, nil, "", nil, err
	}
	if !room.Managed {
		return rooms.Room{}, nil, "", nil, ErrInvalidInput
	}
	executions, err := c.placements.ResolveRoomExecutions(ctx, roomID)
	if err != nil || len(executions) == 0 {
		return rooms.Room{}, nil, "", nil, errors.Join(err, ErrInvalidInput)
	}
	now := c.now().UTC()
	parts := make([]runtimePart, 0, len(executions))
	running := make([]string, 0)
	revision := ""
	setID := uuid.NewString()
	for index, execution := range executions {
		world := execution.World
		driver, target, resolveErr := c.runtimes.DriverTarget(ctx, roomID, world.ID)
		if resolveErr != nil {
			return rooms.Room{}, nil, "", nil, resolveErr
		}
		if !runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityBackupStage) || !runtimedriver.HasTargetCapability(driver, target, runtimedriver.CapabilityBackupRestore) {
			return rooms.Room{}, nil, "", nil, ErrTargetUnavailable
		}
		if target.TopologyRevision != execution.Revision {
			return rooms.Room{}, nil, "", nil, ErrTopologyChanged
		}
		if revision == "" {
			revision = target.TopologyRevision
		} else if revision != target.TopologyRevision {
			return rooms.Room{}, nil, "", nil, ErrTopologyChanged
		}
		status, statusErr := driver.Status(ctx, target)
		if statusErr != nil {
			return rooms.Room{}, nil, "", nil, statusErr
		}
		if status.State == string(shards.RuntimeRunning) || status.State == string(shards.RuntimeStarting) {
			running = append(running, world.ID)
		}
		partID := fmt.Sprintf("backup-%s-%02d", setID, index)
		parts = append(parts, runtimePart{
			driver: driver, target: target, state: status.State,
			part: Part{
				ID: partID, SetID: setID, RoomID: roomID, WorldID: world.ID, WorldName: world.Name, WorldRole: string(world.Role),
				TargetID: target.TargetID, InstallationID: target.InstallationID, Cluster: target.Cluster, Shard: target.Shard,
				TopologyRevision: target.TopologyRevision, FileName: partID + ".zip", Status: PartPending, CreatedAt: now, UpdatedAt: now,
			},
		})
	}
	sort.Strings(running)
	return room, parts, revision, running, nil
}

func (c *Coordinator) initializeSet(room rooms.Room, parts []runtimePart, revision string, running []string, name, kind, sourceJobID, operationID string, lease operationlease.Lease, mode string) (Set, Operation, error) {
	now := c.now().UTC()
	name = strings.TrimSpace(name)
	if name == "" {
		name = "一致性备份 " + now.Format("2006-01-02 15:04:05")
	}
	if len([]rune(name)) > 128 || strings.ContainsAny(name, "\x00\r\n") {
		return Set{}, Operation{}, ErrInvalidInput
	}
	if kind == "" {
		kind = "manual"
	}
	setID := parts[0].part.SetID
	set := Set{
		ID: setID, RoomID: room.ID, RoomName: room.Name, Name: name, Kind: kind, Mode: mode,
		ManifestVersion: manifestVersion, TopologyRevision: revision, Status: StatusCreating,
		OriginalRunningWorlds: append([]string(nil), running...), SourceJobID: sourceJobID, CreatedAt: now, UpdatedAt: now,
	}
	partValues := make([]Part, 0, len(parts))
	for _, part := range parts {
		partValues = append(partValues, part.part)
	}
	created, err := c.store.CreateSet(set, partValues)
	if err != nil {
		return Set{}, Operation{}, err
	}
	operation := Operation{
		ID: operationID, SetID: setID, RoomID: room.ID, Kind: "create", Phase: "planned", Status: OperationRunning,
		TopologyRevision: revision, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken,
		OriginalRunningWorlds: append([]string(nil), running...), CreatedAt: now, UpdatedAt: now,
	}
	operation, err = c.store.CreateOperation(operation)
	return created, operation, err
}

func (c *Coordinator) stagePart(ctx context.Context, current runtimePart, operation Operation, lease *operationlease.Lease, index int) (Part, error) {
	descriptor, part, err := c.stageDescriptor(ctx, current, operation, lease, index)
	if err != nil {
		return part, err
	}
	defer func() {
		_ = current.driver.ReleaseBackup(context.Background(), current.target, c.runtimeOperation(*lease, operation.ID, "release", index, 0), part.ID)
	}()
	return c.collectStagedPart(ctx, current, operation, lease, index, descriptor, part)
}

func (c *Coordinator) stageDescriptor(ctx context.Context, current runtimePart, operation Operation, lease *operationlease.Lease, index int) (runtimedriver.BackupDescriptor, Part, error) {
	part := current.part
	part.Status, part.Failure = PartStaging, ""
	if _, err := c.store.SavePart(part); err != nil {
		return runtimedriver.BackupDescriptor{}, part, err
	}
	step := c.runtimeOperation(*lease, operation.ID, "stage", index, 0)
	descriptor, err := current.driver.StageBackup(ctx, current.target, step, part.ID)
	if err != nil {
		_ = current.driver.ReleaseBackup(context.Background(), current.target, c.runtimeOperation(*lease, operation.ID, "stage-failed-release", index, 0), part.ID)
		failed, cause := c.failPart(part, err)
		return runtimedriver.BackupDescriptor{}, failed, cause
	}
	if descriptor.BackupID != part.ID || descriptor.Size < 1 || descriptor.ContentSize < 1 || descriptor.FileCount < 2 ||
		len(descriptor.SHA256) != 64 || len(descriptor.SharedSHA256) != 64 {
		_ = current.driver.ReleaseBackup(context.Background(), current.target, c.runtimeOperation(*lease, operation.ID, "stage-invalid-release", index, 0), part.ID)
		failed, cause := c.failPart(part, ErrIntegrity)
		return runtimedriver.BackupDescriptor{}, failed, cause
	}
	return descriptor, part, nil
}

func (c *Coordinator) collectStagedPart(ctx context.Context, current runtimePart, operation Operation, lease *operationlease.Lease, index int, descriptor runtimedriver.BackupDescriptor, part Part) (Part, error) {
	path, err := c.partPath(part)
	if err != nil {
		return c.failPart(part, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return c.failPart(part, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".part-*.tmp")
	if err != nil {
		return c.failPart(part, err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	hash := sha256.New()
	written := int64(0)
	for written < descriptor.Size {
		if err := c.renewLease(ctx, lease); err != nil {
			return c.failPart(part, err)
		}
		chunk, readErr := current.driver.ReadBackup(ctx, current.target, part.ID, written)
		if readErr != nil {
			return c.failPart(part, readErr)
		}
		if chunk.Offset != written || chunk.NextOffset != written+int64(len(chunk.Data)) || chunk.NextOffset <= written ||
			chunk.NextOffset > descriptor.Size || chunk.Size != descriptor.Size || !strings.EqualFold(chunk.SHA256, descriptor.SHA256) ||
			chunk.Complete != (chunk.NextOffset == descriptor.Size) {
			return c.failPart(part, ErrIntegrity)
		}
		count, writeErr := io.MultiWriter(temporary, hash).Write(chunk.Data)
		if writeErr != nil || count != len(chunk.Data) {
			return c.failPart(part, errors.Join(writeErr, io.ErrShortWrite))
		}
		written = chunk.NextOffset
	}
	if written != descriptor.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), descriptor.SHA256) {
		return c.failPart(part, ErrIntegrity)
	}
	if err := temporary.Sync(); err != nil {
		return c.failPart(part, err)
	}
	if err := temporary.Close(); err != nil {
		return c.failPart(part, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return c.failPart(part, err)
	}
	published = true
	inspection, err := shardtransfer.InspectBackupArchive(path)
	if err != nil {
		return c.failPart(part, errors.Join(ErrIntegrity, err))
	}
	now := c.now().UTC()
	part.Status, part.Size, part.ContentSize, part.FileCount = PartVerified, descriptor.Size, descriptor.ContentSize, descriptor.FileCount
	part.SHA256, part.SharedSHA256, part.VerifiedAt, part.Failure = strings.ToLower(descriptor.SHA256), strings.ToLower(descriptor.SharedSHA256), &now, ""
	part.ContentKind, part.Restorable = inspection.ContentKind, inspection.Restorable
	part.SessionID, part.LatestSnapshot, part.HasShardIndex = inspection.SessionID, inspection.LatestSnapshot, inspection.HasShardIndex
	part.ValidationError = inspection.ValidationError
	return c.store.SavePart(part)
}

func (c *Coordinator) finalizeSet(setID, sharedSHA string) (Set, error) {
	value, err := c.store.GetSet(setID)
	if err != nil {
		return Set{}, err
	}
	for _, part := range value.Parts {
		if part.Status != PartVerified {
			return Set{}, ErrIncomplete
		}
		compatibleSHA, compatibilityErr := c.partSharedCompatibilitySHA(part)
		if compatibilityErr != nil || !strings.EqualFold(compatibleSHA, sharedSHA) {
			return Set{}, errors.Join(ErrIncomplete, compatibilityErr)
		}
		value.Size += part.Size
		value.ContentSize += part.ContentSize
		value.FileCount += part.FileCount
		if !part.Restorable {
			value.ValidationError = appendValidation(value.ValidationError, part.WorldName+": "+part.ValidationError)
		}
	}
	value.ContentKind = shardtransfer.BackupContentConfigurationOnly
	value.Restorable = len(value.Parts) > 0 && value.ValidationError == ""
	if value.Restorable {
		value.ContentKind = shardtransfer.BackupContentGameSave
	}
	value.SharedSHA256 = strings.ToLower(sharedSHA)
	value.Status, value.Failure = StatusVerified, ""
	now := c.now().UTC()
	value.VerifiedAt, value.UpdatedAt = &now, now
	manifestSHA, err := c.writeManifest(value)
	if err != nil {
		return Set{}, err
	}
	value.ManifestSHA256 = manifestSHA
	return c.store.SaveSet(value)
}

func (c *Coordinator) partSharedCompatibilitySHA(part Part) (string, error) {
	path, err := c.partPath(part)
	if err != nil {
		return "", err
	}
	inspection, err := shardtransfer.InspectBackupArchive(path)
	if err != nil || len(inspection.SharedCompatibilitySHA256) != 64 {
		return "", errors.Join(ErrIntegrity, err)
	}
	return inspection.SharedCompatibilitySHA256, nil
}

func (c *Coordinator) classifySet(value Set) Set {
	value.ValidationError = ""
	value.Restorable = len(value.Parts) > 0
	value.ContentKind = shardtransfer.BackupContentGameSave
	unknown := false
	for index := range value.Parts {
		part := &value.Parts[index]
		path, err := c.partPath(*part)
		if err != nil {
			part.ContentKind, part.Restorable, part.ValidationError = "unknown", false, err.Error()
		} else if inspection, inspectErr := shardtransfer.InspectBackupArchive(path); inspectErr != nil {
			part.ContentKind, part.Restorable, part.ValidationError = "unknown", false, inspectErr.Error()
		} else {
			part.ContentKind, part.Restorable = inspection.ContentKind, inspection.Restorable
			part.SessionID, part.LatestSnapshot, part.HasShardIndex = inspection.SessionID, inspection.LatestSnapshot, inspection.HasShardIndex
			part.ValidationError = inspection.ValidationError
		}
		if !part.Restorable {
			value.Restorable = false
			unknown = unknown || part.ContentKind == "unknown"
			value.ValidationError = appendValidation(value.ValidationError, part.WorldName+": "+part.ValidationError)
		}
	}
	if unknown {
		value.ContentKind = "unknown"
	} else if !value.Restorable {
		value.ContentKind = shardtransfer.BackupContentConfigurationOnly
	}
	return value
}

func appendValidation(current, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "缺少可恢复的世界存档证据"
	}
	if current == "" {
		return message
	}
	return current + "; " + message
}

func (c *Coordinator) failPart(part Part, cause error) (Part, error) {
	part.Status, part.Failure = PartFailed, cause.Error()
	_, _ = c.store.SavePart(part)
	return part, cause
}

func (c *Coordinator) failCreate(value Set, operation Operation, cause error) (Set, error) {
	current, _ := c.store.GetSet(value.ID)
	verified := 0
	for _, part := range current.Parts {
		if part.Status == PartVerified {
			verified++
		}
	}
	current.Status = StatusFailed
	if verified > 0 {
		current.Status = StatusPartial
	}
	current.Failure, current.UpdatedAt = cause.Error(), c.now().UTC()
	saved, saveErr := c.store.SaveSet(current)
	_ = c.saveOperationPhase(&operation, "failed", OperationFailed, cause.Error())
	return saved, errors.Join(cause, saveErr)
}

func (c *Coordinator) stopAll(ctx context.Context, parts []runtimePart, operation Operation, lease *operationlease.Lease) error {
	for index, part := range orderedRuntimeParts(parts, false) {
		status, err := part.driver.Status(ctx, part.target)
		if err != nil {
			return err
		}
		if status.SessionExists || status.State != string(shards.RuntimeStopped) {
			if err := maintenance.Check(ctx); err != nil {
				return err
			}
			if _, err := part.driver.ExecuteShard(ctx, part.target, c.runtimeOperation(*lease, operation.ID, "stop", index, 0), shared.ShardActionStop, stopTimeout); err != nil {
				return err
			}
		}
	}
	deadline := time.NewTimer(stopTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		allStopped := true
		for _, part := range parts {
			status, err := part.driver.Status(ctx, part.target)
			if err != nil {
				return err
			}
			if status.SessionExists || status.State != string(shards.RuntimeStopped) {
				allStopped = false
			}
		}
		if allStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("等待全部分片停止超时")
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) restartWorlds(ctx context.Context, parts []runtimePart, worldIDs []string, operation Operation, lease *operationlease.Lease) error {
	selected := make(map[string]bool, len(worldIDs))
	for _, id := range worldIDs {
		selected[id] = true
	}
	var failures error
	for index, part := range orderedRuntimeParts(parts, true) {
		if !selected[part.part.WorldID] {
			continue
		}
		if err := c.renewLease(ctx, lease); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		_, err := part.driver.ExecuteShard(ctx, part.target, c.runtimeOperation(*lease, operation.ID, "restart", index, 0), shared.ShardActionStart, stopTimeout)
		failures = errors.Join(failures, err)
	}
	return failures
}

func orderedRuntimeParts(parts []runtimePart, masterFirst bool) []runtimePart {
	ordered := append([]runtimePart(nil), parts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		iMaster := ordered[i].part.WorldRole == string(rooms.WorldRoleMaster)
		jMaster := ordered[j].part.WorldRole == string(rooms.WorldRoleMaster)
		if iMaster != jMaster {
			return iMaster == masterFirst
		}
		return ordered[i].part.WorldID < ordered[j].part.WorldID
	})
	return ordered
}

func (c *Coordinator) renewLease(ctx context.Context, lease *operationlease.Lease) error {
	if time.Until(lease.ExpiresAt) > time.Minute {
		return nil
	}
	renewed, err := c.leases.Renew(ctx, *lease, leaseTTL)
	if err == nil {
		*lease = renewed
	}
	return err
}

func (c *Coordinator) runtimeOperation(lease operationlease.Lease, operationID, phase string, index int, offset int64) runtimedriver.Operation {
	key := fmt.Sprintf("b.%s.%d.%s.%d.%d", operationID, lease.FencingToken, phase, index, offset)
	expires := lease.ExpiresAt.UTC()
	return runtimedriver.Operation{ID: key, Key: key, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, LeaseExpiresAt: &expires}
}

func (c *Coordinator) saveOperationPhase(value *Operation, phase string, status OperationStatus, failure string) error {
	value.Phase, value.Status, value.Failure, value.UpdatedAt = phase, status, failure, c.now().UTC()
	saved, err := c.store.SaveOperation(*value)
	if err == nil {
		*value = saved
	}
	return err
}

func (c *Coordinator) partPath(part Part) (string, error) {
	if part.SetID == "" || part.FileName != part.ID+".zip" || strings.ContainsAny(part.SetID+part.ID+part.FileName, "\x00/\\\r\n") {
		return "", ErrInvalidInput
	}
	path := filepath.Join(c.root, "sets", part.SetID, "parts", part.FileName)
	if !containedPath(c.root, path) {
		return "", ErrInvalidInput
	}
	return path, nil
}

func (c *Coordinator) writeManifest(value Set) (string, error) {
	value.ManifestSHA256 = ""
	sort.Slice(value.Parts, func(i, j int) bool { return value.Parts[i].WorldID < value.Parts[j].WorldID })
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	manifest := struct {
		SHA256 string `json:"sha256"`
		Set    Set    `json:"set"`
	}{SHA256: checksum, Set: value}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	directory := filepath.Join(c.root, "sets", value.ID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, ".manifest-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, "manifest.json")); err != nil {
		return "", err
	}
	return checksum, nil
}

func containedPath(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}
