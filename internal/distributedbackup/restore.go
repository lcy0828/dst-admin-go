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

	"dont/internal/operationlease"
	"dont/internal/roomops"
	"dont/internal/runtimedriver"
	"dont/internal/shardtransfer"

	"github.com/google/uuid"
)

type restorePart struct {
	runtimePart
	source        Part
	descriptor    runtimedriver.BackupDescriptor
	restoreID     string
	publishShared bool
}

func (c *Coordinator) Restore(ctx context.Context, setID, confirmation, sourceJobID string) (result RestoreResult, returnErr error) {
	backupSet, err := c.store.GetSet(setID)
	if err != nil {
		return RestoreResult{}, err
	}
	backupSet = c.classifySet(backupSet)
	if !backupSet.Restorable {
		if backupSet.ContentKind == "unknown" {
			return RestoreResult{}, errors.Join(ErrIntegrity, errors.New(backupSet.ValidationError))
		}
		return RestoreResult{}, errors.Join(ErrNotRestorable, errors.New(backupSet.ValidationError))
	}
	ctx, releaseRoom, err := roomops.Acquire(ctx, backupSet.RoomID)
	if err != nil {
		return RestoreResult{}, err
	}
	defer releaseRoom()
	operationID := uuid.NewString()
	lease, err := c.leases.Acquire(ctx, backupSet.RoomID, "backup-set.restore:"+operationID, leaseTTL)
	if err != nil {
		return RestoreResult{}, err
	}
	defer c.leases.Release(lease)
	room, currentParts, revision, running, err := c.plan(ctx, backupSet.RoomID)
	if err != nil {
		return RestoreResult{}, err
	}
	if confirmation != room.Name {
		return RestoreResult{}, ErrInvalidInput
	}
	if err := c.verifySet(backupSet, currentParts, revision); err != nil {
		return RestoreResult{}, err
	}
	now := c.now().UTC()
	operation := Operation{
		ID: operationID, SetID: setID, RoomID: room.ID, Kind: "restore", Phase: "planned", Status: OperationRunning,
		TopologyRevision: revision, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken,
		OriginalRunningWorlds: append([]string(nil), running...), CreatedAt: now, UpdatedAt: now,
	}
	operation, err = c.store.CreateOperation(operation)
	if err != nil {
		return RestoreResult{}, err
	}
	result = RestoreResult{SetID: setID, OperationID: operationID, Warnings: []string{}}
	defer func() {
		if restartErr := c.restartWorlds(context.Background(), currentParts, running, operation, &lease); restartErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore pre-restore running shards: %w", restartErr))
		}
	}()
	if err := c.saveOperationPhase(&operation, "stopping", OperationRunning, ""); err != nil {
		return result, err
	}
	if err := c.stopAll(ctx, currentParts, operation, &lease); err != nil {
		return result, c.failRestore(&operation, nil, &lease, err)
	}
	if err := c.saveOperationPhase(&operation, "protecting", OperationRunning, ""); err != nil {
		return result, err
	}
	protectionID := uuid.NewString()
	protectionParts := rekeyParts(currentParts, protectionID, now)
	protection, protectionOperation, err := c.initializeSet(
		room, protectionParts, revision, running, "恢复前保护备份 "+now.Format("2006-01-02 15:04:05"), "protection", sourceJobID,
		uuid.NewString(), lease, ModeCold,
	)
	if err != nil {
		return result, c.failRestore(&operation, nil, &lease, err)
	}
	protection, err = c.createWithPlan(ctx, protection, protectionOperation, protectionParts, &lease, false)
	if err != nil {
		return result, c.failRestore(&operation, nil, &lease, fmt.Errorf("create distributed protection backup: %w", err))
	}
	result.ProtectionSetID, operation.ProtectionSetID = protection.ID, protection.ID
	if err := c.saveOperationPhase(&operation, "preparing", OperationRunning, ""); err != nil {
		return result, err
	}
	plans, err := c.restorePlans(backupSet, currentParts, operationID)
	if err != nil {
		return result, c.failRestore(&operation, nil, &lease, err)
	}
	prepared := make([]restorePart, 0, len(plans))
	for index, plan := range plans {
		if err := c.renewLease(ctx, &lease); err != nil {
			return result, c.failRestore(&operation, prepared, &lease, err)
		}
		if err := c.transferRestore(ctx, plan, operation, &lease, index); err != nil {
			prepared = append(prepared, plan)
			return result, c.failRestore(&operation, prepared, &lease, err)
		}
		prepared = append(prepared, plan)
	}
	if err := c.saveOperationPhase(&operation, "prepared", OperationRunning, ""); err != nil {
		return result, c.failRestore(&operation, prepared, &lease, err)
	}
	if err := c.saveOperationPhase(&operation, "publishing", OperationRunning, ""); err != nil {
		return result, c.failRestore(&operation, prepared, &lease, err)
	}
	for index, plan := range plans {
		if err := c.renewLease(ctx, &lease); err != nil {
			return result, c.failRestore(&operation, prepared, &lease, err)
		}
		if _, err := plan.driver.PublishRestore(ctx, plan.target, c.runtimeOperation(lease, operation.ID, "publish", index, 0), plan.restoreID, plan.publishShared); err != nil {
			return result, c.failRestore(&operation, prepared, &lease, err)
		}
	}
	if err := c.saveOperationPhase(&operation, "published", OperationRunning, ""); err != nil {
		_ = c.saveOperationPhase(&operation, "published", OperationRecoveryRequired, err.Error())
		return result, err
	}
	for index, plan := range plans {
		if _, err := plan.driver.CompleteRestore(ctx, plan.target, c.runtimeOperation(lease, operation.ID, "complete", index, 0), plan.restoreID); err != nil {
			result.Warnings = append(result.Warnings, plan.source.WorldName+" 恢复清理待重试: "+err.Error())
		}
	}
	status, failure := OperationSucceeded, ""
	if len(result.Warnings) > 0 {
		status, failure = OperationRecoveryRequired, strings.Join(result.Warnings, "; ")
	}
	if err := c.saveOperationPhase(&operation, "completed", status, failure); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Coordinator) verifySet(value Set, current []runtimePart, revision string) error {
	value = c.classifySet(value)
	if !value.Restorable {
		if value.ContentKind == "unknown" {
			return errors.Join(ErrIntegrity, errors.New(value.ValidationError))
		}
		return errors.Join(ErrNotRestorable, errors.New(value.ValidationError))
	}
	if value.Status != StatusVerified || value.VerifiedAt == nil || value.ManifestVersion != manifestVersion || len(value.Parts) != len(current) ||
		value.TopologyRevision != revision || len(value.ManifestSHA256) != 64 || len(value.SharedSHA256) != 64 {
		return ErrIncomplete
	}
	manifestPath := filepath.Join(c.root, "sets", value.ID, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	var manifest struct {
		SHA256 string `json:"sha256"`
		Set    Set    `json:"set"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || !strings.EqualFold(manifest.SHA256, value.ManifestSHA256) {
		return ErrIntegrity
	}
	manifest.Set.ManifestSHA256 = ""
	sort.Slice(manifest.Set.Parts, func(i, j int) bool { return manifest.Set.Parts[i].WorldID < manifest.Set.Parts[j].WorldID })
	canonical, err := json.MarshalIndent(manifest.Set, "", "  ")
	if err != nil {
		return ErrIntegrity
	}
	sum := sha256.Sum256(canonical)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), value.ManifestSHA256) {
		return ErrIntegrity
	}
	byWorld := make(map[string]runtimePart, len(current))
	for _, item := range current {
		byWorld[item.part.WorldID] = item
	}
	for _, part := range value.Parts {
		target, exists := byWorld[part.WorldID]
		if !exists || part.Status != PartVerified || part.TopologyRevision != revision || part.TargetID != target.target.TargetID ||
			part.InstallationID != target.target.InstallationID || part.Cluster != target.target.Cluster || part.Shard != target.target.Shard ||
			!strings.EqualFold(part.SharedSHA256, value.SharedSHA256) {
			return ErrTopologyChanged
		}
		path, err := c.partPath(part)
		if err != nil {
			return err
		}
		if err := verifyRegularFile(path, part.Size, part.SHA256); err != nil {
			return err
		}
		inspection, inspectErr := shardtransfer.InspectBackupArchive(path)
		if inspectErr != nil || !inspection.Restorable {
			return errors.Join(ErrNotRestorable, inspectErr, errors.New(inspection.ValidationError))
		}
	}
	return nil
}

func (c *Coordinator) restorePlans(value Set, current []runtimePart, operationID string) ([]restorePart, error) {
	byWorld := make(map[string]runtimePart, len(current))
	for _, item := range current {
		byWorld[item.part.WorldID] = item
	}
	parts := append([]Part(nil), value.Parts...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].WorldID < parts[j].WorldID })
	leaders := make(map[string]bool)
	plans := make([]restorePart, 0, len(parts))
	compactOperation := strings.ReplaceAll(operationID, "-", "")
	for index, source := range parts {
		currentPart, exists := byWorld[source.WorldID]
		if !exists {
			return nil, ErrTopologyChanged
		}
		key := source.TargetID + "\x00" + source.InstallationID + "\x00" + source.Cluster
		publishShared := !leaders[key]
		leaders[key] = true
		restoreID := fmt.Sprintf("restore-%s-%02d", compactOperation, index)
		plans = append(plans, restorePart{
			runtimePart: currentPart, source: source, restoreID: restoreID, publishShared: publishShared,
			descriptor: runtimedriver.BackupDescriptor{
				BackupID: restoreID, Size: source.Size, ContentSize: source.ContentSize, FileCount: source.FileCount,
				SHA256: source.SHA256, SharedSHA256: source.SharedSHA256,
			},
		})
	}
	return plans, nil
}

func (c *Coordinator) transferRestore(ctx context.Context, plan restorePart, operation Operation, lease *operationlease.Lease, index int) error {
	if err := plan.driver.BeginRestore(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "begin", index, 0), plan.descriptor); err != nil {
		return err
	}
	path, err := c.partPath(plan.source)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, shardtransfer.MaxChunkBytes)
	offset := int64(0)
	for offset < plan.descriptor.Size {
		if err := c.renewLease(ctx, lease); err != nil {
			return err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			next, writeErr := plan.driver.WriteRestore(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "write", index, offset), plan.descriptor, offset, buffer[:read])
			if writeErr != nil {
				return writeErr
			}
			if next != offset+int64(read) {
				return ErrIntegrity
			}
			offset = next
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	if offset != plan.descriptor.Size {
		return ErrIntegrity
	}
	return plan.driver.PrepareRestore(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "prepare", index, 0), plan.restoreID)
}

func (c *Coordinator) failRestore(operation *Operation, plans []restorePart, lease *operationlease.Lease, cause error) error {
	rollbackErr := c.rollbackRestore(context.Background(), plans, *operation, lease)
	status := OperationRolledBack
	if rollbackErr != nil {
		status = OperationRecoveryRequired
	}
	failure := cause.Error()
	if rollbackErr != nil {
		failure += "; rollback: " + rollbackErr.Error()
	}
	_ = c.saveOperationPhase(operation, "rolled_back", status, failure)
	return errors.Join(cause, rollbackErr)
}

func (c *Coordinator) rollbackRestore(ctx context.Context, plans []restorePart, operation Operation, lease *operationlease.Lease) error {
	var failures error
	for index := len(plans) - 1; index >= 0; index-- {
		if err := c.renewLease(ctx, lease); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		plan := plans[index]
		err := plan.driver.RollbackRestore(ctx, plan.target, c.runtimeOperation(*lease, operation.ID, "rollback", index, 0), plan.restoreID)
		failures = errors.Join(failures, err)
	}
	return failures
}

func rekeyParts(values []runtimePart, setID string, now time.Time) []runtimePart {
	result := make([]runtimePart, len(values))
	for index, value := range values {
		value.part.ID = fmt.Sprintf("backup-%s-%02d", setID, index)
		value.part.SetID = setID
		value.part.FileName = value.part.ID + ".zip"
		value.part.Status, value.part.Failure = PartPending, ""
		value.part.Size, value.part.ContentSize, value.part.FileCount = 0, 0, 0
		value.part.SHA256, value.part.SharedSHA256, value.part.VerifiedAt = "", "", nil
		value.part.CreatedAt, value.part.UpdatedAt = now, now
		result[index] = value
	}
	return result
}

func verifyRegularFile(path string, expectedSize int64, expectedSHA string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return errors.Join(ErrIntegrity, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedSHA) {
		return ErrIntegrity
	}
	return nil
}
