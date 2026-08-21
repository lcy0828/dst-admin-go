package saveimport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dont/internal/distributedbackup"
	"dont/internal/rooms"
)

func (s *Service) applyCoordinatedReplacement(ctx context.Context, id, jobID string, value Session, candidate Candidate, request ApplyRequest) (result ApplyResult, resultErr error) {
	room, err := s.rooms.Room(request.TargetRoomID)
	if err != nil {
		return ApplyResult{}, err
	}
	if !room.Managed {
		return ApplyResult{}, ErrTargetNotManaged
	}
	if request.Confirmation != room.Name {
		return ApplyResult{}, ErrConfirmation
	}
	targetRoot := filepath.Join(s.config.SaveRoot, room.DirectoryName)
	if !contained(s.config.SaveRoot, targetRoot) || !regularDirectory(targetRoot) {
		return ApplyResult{}, ErrTargetNotManaged
	}
	contentRoot := filepath.Join(s.config.ImportRoot, id, "content")
	sourceRoot := filepath.Join(contentRoot, filepath.FromSlash(candidate.Root))
	if !contained(contentRoot, sourceRoot) || !regularDirectory(sourceRoot) {
		return ApplyResult{}, ErrUnsafeArchive
	}
	if value.Manifest == nil {
		return ApplyResult{}, ErrImportNotReady
	}
	if err := requireImportSpace(s.config.ImportRoot, value.Manifest.ContentSize); err != nil {
		return ApplyResult{}, err
	}
	staging, err := os.MkdirTemp(filepath.Join(s.config.ImportRoot, id), ".apply-")
	if err != nil {
		return ApplyResult{}, err
	}
	defer os.RemoveAll(staging)
	if err := copyDirectory(ctx, sourceRoot, staging); err != nil {
		return ApplyResult{}, err
	}
	if err := canonicalizeDSTNames(staging); err != nil {
		return ApplyResult{}, err
	}
	if strings.TrimSpace(request.RoomName) != "" {
		if err := setClusterName(staging, request.RoomName); err != nil {
			return ApplyResult{}, err
		}
	}
	if err := s.applyTokenPolicy(staging, targetRoot, request); err != nil {
		return ApplyResult{}, err
	}
	if err := preserveNetworkConfiguration(targetRoot, staging); err != nil {
		return ApplyResult{}, err
	}
	installedMods, warnings, err := s.reconcileMods(ctx, candidate, request.ModPolicy)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := verifyStagedRoom(staging, request.AllowPartial); err != nil {
		return ApplyResult{}, err
	}
	worlds, err := s.coordinatedWorldMapping(room.ID, targetRoot, staging)
	if err != nil {
		return ApplyResult{}, err
	}
	backupSet, err := s.coordinated.ImportDirectory(ctx, distributedbackup.DirectoryImportRequest{
		RoomID: room.ID, SourceRoot: staging, Name: value.Name, Kind: "import", SourceJobID: jobID, Worlds: worlds,
	})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("prepare imported room: %w", err)
	}
	if err := s.store.BeginCoordinatedApply(id, room.ID, backupSet.ID); err != nil {
		return ApplyResult{}, err
	}
	recoveryPending := false
	defer func() {
		if resultErr == nil || recoveryPending {
			return
		}
		if markErr := s.store.MarkApplyFailed(id, ErrorCode(resultErr), resultErr.Error()); markErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("record coordinated import failure: %w", markErr))
		}
	}()
	restored, restoreErr := s.coordinated.Restore(ctx, backupSet.ID, room.Name, jobID)
	operation, err := s.coordinatedRestoreOperation(room.ID, backupSet.ID, restored.OperationID)
	if err != nil {
		recoveryPending = true
		failure := errors.Join(fmt.Errorf("inspect imported room restore: %w", err), restoreErr)
		return ApplyResult{}, errors.Join(failure, s.markCoordinatedRecoveryBlocked(id, failure))
	}
	if operation == nil {
		if restoreErr != nil {
			return ApplyResult{}, fmt.Errorf("restore imported room: %w", restoreErr)
		}
		recoveryPending = true
		failure := errors.New("restore imported room operation was not recorded")
		return ApplyResult{}, errors.Join(failure, s.markCoordinatedRecoveryBlocked(id, failure))
	}
	if operation.Status == distributedbackup.OperationRunning || operation.Status == distributedbackup.OperationRecoveryRequired {
		recovered, recoverErr := s.coordinated.RecoverOperation(context.Background(), operation.ID)
		if recoverErr != nil {
			recoveryPending = true
			failure := errors.Join(fmt.Errorf("recover imported room restore: %w", recoverErr), restoreErr)
			return ApplyResult{}, errors.Join(failure, s.markCoordinatedRecoveryBlocked(id, failure))
		}
		operation = &recovered
	}
	switch operation.Status {
	case distributedbackup.OperationSucceeded:
		// A recovery may have completed an operation after Restore returned an
		// error, so the durable operation status is authoritative here.
	case distributedbackup.OperationRolledBack, distributedbackup.OperationFailed:
		if restoreErr == nil {
			restoreErr = fmt.Errorf("restore operation ended with status %s", operation.Status)
		}
		return ApplyResult{}, fmt.Errorf("restore imported room: %w", restoreErr)
	default:
		recoveryPending = true
		failure := fmt.Errorf("restore imported room operation remains in status %s", operation.Status)
		return ApplyResult{}, errors.Join(failure, s.markCoordinatedRecoveryBlocked(id, failure))
	}
	if _, err := s.store.MarkCoordinatedApplied(id); err != nil {
		return ApplyResult{}, err
	}
	now := s.now().UTC()
	return ApplyResult{
		ImportID: id, CandidateID: candidate.ID, Mode: request.Mode, RoomID: room.ID,
		DirectoryName: room.DirectoryName, RoomName: room.Name, ProtectionBackupID: restored.ProtectionSetID,
		InstalledMods: installedMods, Warnings: warnings, AppliedAt: now,
	}, nil
}

func (s *Service) coordinatedRestoreOperation(roomID, setID, operationID string) (*distributedbackup.Operation, error) {
	operations, err := s.coordinated.Operations(roomID)
	if err != nil {
		return nil, err
	}
	operationID = strings.TrimSpace(operationID)
	for index := range operations {
		operation := &operations[index]
		if operationID != "" {
			if operation.ID == operationID {
				return operation, nil
			}
			continue
		}
		if operation.SetID == setID && operation.Kind == "restore" {
			return operation, nil
		}
	}
	return nil, nil
}

func (s *Service) markCoordinatedRecoveryBlocked(id string, cause error) error {
	message := "存档替换仍需恢复：" + cause.Error()
	if err := s.store.MarkRecoveryBlocked(id, ErrorCode(cause), message); err != nil {
		return fmt.Errorf("record coordinated recovery state: %w", err)
	}
	return nil
}

func (s *Service) coordinatedWorldMapping(roomID, targetRoot, importedRoot string) ([]distributedbackup.DirectoryImportWorld, error) {
	logicalWorlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return nil, err
	}
	targetWorlds, err := readNetworkWorlds(targetRoot)
	if err != nil {
		return nil, err
	}
	importedWorlds, err := readNetworkWorlds(importedRoot)
	if err != nil {
		return nil, err
	}
	if len(logicalWorlds) == 0 || len(targetWorlds) != len(logicalWorlds) || len(importedWorlds) != len(logicalWorlds) {
		return nil, ErrWorldMismatch
	}
	logicalByDirectory := make(map[string]rooms.World, len(logicalWorlds))
	for _, world := range logicalWorlds {
		logicalByDirectory[strings.ToLower(world.DirectoryName)] = world
	}
	usedImported := make(map[string]bool, len(importedWorlds))
	result := make([]distributedbackup.DirectoryImportWorld, 0, len(logicalWorlds))
	for _, target := range targetWorlds {
		logical, exists := logicalByDirectory[strings.ToLower(target.directory)]
		if !exists {
			return nil, ErrWorldMismatch
		}
		imported := matchNetworkWorld(importedWorlds, target, usedImported)
		if imported == nil {
			return nil, ErrWorldMismatch
		}
		usedImported[imported.directory] = true
		result = append(result, distributedbackup.DirectoryImportWorld{WorldID: logical.ID, DirectoryName: imported.directory})
	}
	if len(usedImported) != len(importedWorlds) {
		return nil, ErrWorldMismatch
	}
	return result, nil
}

func (s *Service) recoverCoordinatedApply(record importRecord) error {
	if s.coordinated == nil || strings.TrimSpace(record.ApplyBackupSetID) == "" || strings.TrimSpace(record.ApplyRoomID) == "" {
		return s.store.MarkApplyFailed(record.ID, "SERVER_RESTARTED", "服务重启中断了存档替换，请检查目标房间后重试")
	}
	operations, err := s.coordinated.Operations(record.ApplyRoomID)
	if err != nil {
		return err
	}
	var latest *distributedbackup.Operation
	for index := range operations {
		if operations[index].SetID == record.ApplyBackupSetID && operations[index].Kind == "restore" {
			latest = &operations[index]
			break
		}
	}
	if latest == nil {
		return s.store.MarkApplyFailed(record.ID, "SERVER_RESTARTED", "服务重启发生在存档发布前，未修改目标房间")
	}
	if latest.Status == distributedbackup.OperationRunning || latest.Status == distributedbackup.OperationRecoveryRequired {
		recovered, recoverErr := s.coordinated.RecoverOperation(context.Background(), latest.ID)
		if recoverErr != nil {
			message := "存档替换恢复尚未完成：" + recoverErr.Error()
			return s.store.MarkRecoveryBlocked(record.ID, ErrorCode(recoverErr), message)
		}
		latest = &recovered
	}
	if latest.Status == distributedbackup.OperationSucceeded {
		_, err := s.store.MarkCoordinatedApplied(record.ID)
		return err
	}
	return s.store.MarkApplyFailed(record.ID, "SERVER_RESTARTED", "服务重启后已回滚未完成的存档替换，请重新执行")
}
