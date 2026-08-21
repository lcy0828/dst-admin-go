package shardtransfer

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type BackupDescriptor struct {
	BackupID     string `json:"backupId"`
	Size         int64  `json:"size"`
	ContentSize  int64  `json:"contentSize"`
	FileCount    int    `json:"fileCount"`
	SHA256       string `json:"sha256"`
	SharedSHA256 string `json:"sharedSha256"`
	Cluster      string `json:"cluster"`
	Shard        string `json:"shard"`
	Phase        string `json:"phase,omitempty"`
}

const (
	BackupContentGameSave          = "game-save"
	BackupContentConfigurationOnly = "configuration-only"
)

// BackupInspection describes whether an otherwise valid backup archive also
// contains the minimum DST save evidence required for a world restore.
type BackupInspection struct {
	ContentSize     int64
	FileCount       int
	SharedSHA256    string
	ContentKind     string
	Restorable      bool
	SessionID       string
	LatestSnapshot  string
	HasShardIndex   bool
	ValidationError string
}

type RestoreReceipt struct {
	BackupID       string   `json:"backupId"`
	Cluster        string   `json:"cluster"`
	Shard          string   `json:"shard"`
	Phase          string   `json:"phase"`
	PublishShared  bool     `json:"publishShared"`
	OriginalShared []string `json:"originalShared,omitempty"`
	IncomingShared []string `json:"incomingShared,omitempty"`
	RecoveryRef    string   `json:"recoveryRef"`
}

func (m *Manager) PrepareBackup(ctx context.Context, id, cluster, shard string) (BackupDescriptor, error) {
	value, err := m.PrepareExport(ctx, id, cluster, shard)
	if err != nil {
		return BackupDescriptor{}, err
	}
	inspection, err := InspectBackupArchive(m.exportPath(id))
	if err != nil {
		return BackupDescriptor{}, err
	}
	return BackupDescriptor{
		BackupID: id, Size: value.Size, ContentSize: inspection.ContentSize, FileCount: inspection.FileCount,
		SHA256: value.SHA256, SharedSHA256: inspection.SharedSHA256, Cluster: cluster, Shard: shard, Phase: "staged",
	}, nil
}

func (m *Manager) ReadBackup(ctx context.Context, id string, offset int64) (Chunk, error) {
	return m.ReadExport(ctx, id, offset)
}

func (m *Manager) ReleaseBackup(id string) error { return m.ReleaseExport(id) }

func (m *Manager) BeginRestore(descriptor BackupDescriptor) (BackupDescriptor, error) {
	if err := validateBackupDescriptor(descriptor); err != nil {
		return BackupDescriptor{}, err
	}
	if current, err := m.restoreDescriptor(descriptor.BackupID); err == nil {
		if sameBackupDescriptor(current, descriptor) {
			return current, nil
		}
		return BackupDescriptor{}, ErrConflict
	} else if !os.IsNotExist(err) {
		return BackupDescriptor{}, err
	}
	value := descriptor
	value.Phase = "receiving"
	file, err := os.OpenFile(m.restoreArchivePath(value.BackupID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return BackupDescriptor{}, err
	}
	if err := file.Close(); err != nil {
		return BackupDescriptor{}, err
	}
	if err := writeJSON(m.restoreMetaPath(value.BackupID), value); err != nil {
		_ = os.Remove(m.restoreArchivePath(value.BackupID))
		return BackupDescriptor{}, err
	}
	return value, nil
}

func (m *Manager) WriteRestore(id string, offset int64, data []byte) (int64, error) {
	if !migrationID.MatchString(id) || offset < 0 || len(data) < 1 || len(data) > MaxChunkBytes {
		return 0, ErrInvalidRequest
	}
	descriptor, err := m.restoreDescriptor(id)
	if err != nil || descriptor.Phase != "receiving" || offset > descriptor.Size || int64(len(data)) > descriptor.Size-offset {
		return 0, ErrInvalidRequest
	}
	file, err := os.OpenFile(m.restoreArchivePath(id), os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != offset {
		if err != nil {
			return 0, err
		}
		return info.Size(), ErrConflict
	}
	written, err := file.WriteAt(data, offset)
	if err != nil || written != len(data) {
		return offset + int64(written), errors.Join(err, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return offset + int64(written), err
	}
	return offset + int64(written), nil
}

func (m *Manager) PrepareRestore(ctx context.Context, id string) (BackupDescriptor, error) {
	descriptor, err := m.restoreDescriptor(id)
	if err != nil {
		return BackupDescriptor{}, err
	}
	if descriptor.Phase == "completed" {
		return descriptor, nil
	}
	if descriptor.Phase == "prepared" || descriptor.Phase == "published" {
		if err := m.validateRestoreStage(descriptor); err == nil || descriptor.Phase == "published" {
			return descriptor, nil
		}
		return BackupDescriptor{}, ErrIntegrity
	}
	if descriptor.Phase != "receiving" {
		return BackupDescriptor{}, ErrConflict
	}
	actual, err := describeFile(id, m.restoreArchivePath(id))
	if err != nil || actual.Size != descriptor.Size || !strings.EqualFold(actual.SHA256, descriptor.SHA256) {
		return BackupDescriptor{}, ErrIntegrity
	}
	stage, err := m.restoreStagePath(descriptor.Cluster, id)
	if err != nil {
		return BackupDescriptor{}, err
	}
	if err := os.RemoveAll(stage); err != nil {
		return BackupDescriptor{}, err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return BackupDescriptor{}, err
	}
	if err := extractArchive(ctx, m.restoreArchivePath(id), stage); err != nil {
		_ = os.RemoveAll(stage)
		return BackupDescriptor{}, err
	}
	if err := m.validateRestoreStage(descriptor); err != nil {
		_ = os.RemoveAll(stage)
		return BackupDescriptor{}, err
	}
	marker := filepath.Join(stage, "shard", ".dst-admin-restore-id")
	if err := os.WriteFile(marker, []byte(id+"\n"), 0o600); err != nil {
		_ = os.RemoveAll(stage)
		return BackupDescriptor{}, err
	}
	descriptor.Phase = "prepared"
	if err := writeJSON(m.restoreMetaPath(id), descriptor); err != nil {
		_ = os.RemoveAll(stage)
		return BackupDescriptor{}, err
	}
	return descriptor, nil
}

func (m *Manager) PublishRestore(id string, publishShared bool) (string, error) {
	descriptor, err := m.restoreDescriptor(id)
	if err != nil {
		return "", err
	}
	if descriptor.Phase == "published" || descriptor.Phase == "completed" {
		receipt, readErr := m.restoreReceipt(id)
		return receipt.RecoveryRef, readErr
	}
	if descriptor.Phase != "prepared" {
		return "", ErrConflict
	}
	roomPath, err := m.targetRoomPath(descriptor.Cluster)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(roomPath, 0o750); err != nil {
		return "", err
	}
	receipt, err := m.prepareRestoreReceipt(descriptor, publishShared, roomPath)
	if err != nil {
		return "", err
	}
	if err := m.publishRestoreFiles(descriptor, receipt, roomPath); err != nil {
		return receipt.RecoveryRef, err
	}
	receipt.Phase = "published"
	if err := writeJSON(m.restoreReceiptPath(id), receipt); err != nil {
		return receipt.RecoveryRef, err
	}
	descriptor.Phase = "published"
	if err := writeJSON(m.restoreMetaPath(id), descriptor); err != nil {
		return receipt.RecoveryRef, err
	}
	return receipt.RecoveryRef, nil
}

func (m *Manager) RollbackRestore(id string) error {
	receipt, err := m.restoreReceipt(id)
	if os.IsNotExist(err) {
		var stageErr error
		if descriptor, descriptorErr := m.restoreDescriptor(id); descriptorErr == nil {
			if stage, pathErr := m.restoreStagePath(descriptor.Cluster, id); pathErr != nil {
				stageErr = pathErr
			} else {
				stageErr = os.RemoveAll(stage)
			}
		}
		return errors.Join(stageErr, removeIfExists(m.restoreArchivePath(id)), removeIfExists(m.restoreMetaPath(id)))
	}
	if err != nil {
		return err
	}
	if receipt.Phase == "completing" || receipt.Phase == "completed" {
		return ErrConflict
	}
	roomPath, err := m.targetRoomPath(receipt.Cluster)
	if err != nil {
		return err
	}
	stage, err := m.restoreStagePath(receipt.Cluster, id)
	if err != nil {
		return err
	}
	recoveryRoot := filepath.Join(m.saveRoot, filepath.FromSlash(receipt.RecoveryRef))
	targetShard := filepath.Join(roomPath, receipt.Shard)
	marker, _ := os.ReadFile(filepath.Join(targetShard, ".dst-admin-restore-id"))
	if strings.TrimSpace(string(marker)) == id {
		if err := os.RemoveAll(targetShard); err != nil {
			return err
		}
	}
	recoveryShard := filepath.Join(recoveryRoot, "shard")
	if _, statErr := os.Lstat(recoveryShard); statErr == nil {
		if _, targetErr := os.Lstat(targetShard); targetErr == nil {
			return ErrConflict
		} else if !os.IsNotExist(targetErr) {
			return targetErr
		}
		if err := os.Rename(recoveryShard, targetShard); err != nil {
			return err
		}
	}
	if receipt.PublishShared {
		for _, name := range unionNames(receipt.OriginalShared, receipt.IncomingShared) {
			target := filepath.Join(roomPath, name)
			original := filepath.Join(recoveryRoot, "shared", name)
			if regularExists(original) {
				if err := publishRegular(original, target); err != nil {
					return err
				}
			} else if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return errors.Join(os.RemoveAll(recoveryRoot), os.RemoveAll(stage), removeIfExists(m.restoreReceiptPath(id)), removeIfExists(m.restoreArchivePath(id)), removeIfExists(m.restoreMetaPath(id)))
}

func (m *Manager) CompleteRestore(id string) (string, error) {
	receipt, err := m.restoreReceipt(id)
	if err != nil {
		return "", err
	}
	if receipt.Phase == "completed" {
		return receipt.RecoveryRef, nil
	}
	descriptor, err := m.restoreDescriptor(id)
	if err != nil {
		return "", err
	}
	roomPath, err := m.targetRoomPath(receipt.Cluster)
	if err != nil {
		return "", err
	}
	stage, err := m.restoreStagePath(receipt.Cluster, id)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(roomPath, receipt.Shard, ".dst-admin-restore-id")
	if receipt.Phase != "completing" {
		if receipt.Phase != "published" {
			return "", ErrConflict
		}
		data, readErr := os.ReadFile(marker)
		if readErr != nil || strings.TrimSpace(string(data)) != id {
			return "", ErrConflict
		}
		receipt.Phase = "completing"
		if err := writeJSON(m.restoreReceiptPath(id), receipt); err != nil {
			return "", err
		}
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	recoveryRoot := filepath.Join(m.saveRoot, filepath.FromSlash(receipt.RecoveryRef))
	if err := errors.Join(os.RemoveAll(recoveryRoot), os.RemoveAll(stage), removeIfExists(m.restoreArchivePath(id))); err != nil {
		return receipt.RecoveryRef, err
	}
	descriptor.Phase = "completed"
	if err := writeJSON(m.restoreMetaPath(id), descriptor); err != nil {
		return receipt.RecoveryRef, err
	}
	receipt.Phase = "completed"
	if err := writeJSON(m.restoreReceiptPath(id), receipt); err != nil {
		return receipt.RecoveryRef, err
	}
	return receipt.RecoveryRef, nil
}

func (m *Manager) prepareRestoreReceipt(descriptor BackupDescriptor, publishShared bool, roomPath string) (RestoreReceipt, error) {
	if current, err := m.restoreReceipt(descriptor.BackupID); err == nil {
		if current.Cluster != descriptor.Cluster || current.Shard != descriptor.Shard || current.PublishShared != publishShared {
			return RestoreReceipt{}, ErrConflict
		}
		return current, nil
	} else if !os.IsNotExist(err) {
		return RestoreReceipt{}, err
	}
	recoveryRef := filepath.ToSlash(filepath.Join(descriptor.Cluster, ".dst-admin-restores", descriptor.BackupID))
	recoveryRoot := filepath.Join(m.saveRoot, filepath.FromSlash(recoveryRef))
	if err := os.MkdirAll(filepath.Join(recoveryRoot, "shared"), 0o700); err != nil {
		return RestoreReceipt{}, err
	}
	receipt := RestoreReceipt{
		BackupID: descriptor.BackupID, Cluster: descriptor.Cluster, Shard: descriptor.Shard,
		Phase: "publishing", PublishShared: publishShared, RecoveryRef: recoveryRef,
	}
	stage, err := m.restoreStagePath(descriptor.Cluster, descriptor.BackupID)
	if err != nil {
		return RestoreReceipt{}, err
	}
	if publishShared {
		for _, name := range sortedSharedNames() {
			if regularExists(filepath.Join(roomPath, name)) {
				receipt.OriginalShared = append(receipt.OriginalShared, name)
				if err := copyRegular(filepath.Join(roomPath, name), filepath.Join(recoveryRoot, "shared", name), 0o600); err != nil {
					return RestoreReceipt{}, err
				}
			}
			if regularExists(filepath.Join(stage, "shared", name)) {
				receipt.IncomingShared = append(receipt.IncomingShared, name)
			}
		}
	}
	if err := writeJSON(m.restoreReceiptPath(descriptor.BackupID), receipt); err != nil {
		return RestoreReceipt{}, err
	}
	return receipt, nil
}

func (m *Manager) publishRestoreFiles(descriptor BackupDescriptor, receipt RestoreReceipt, roomPath string) error {
	stage, err := m.restoreStagePath(descriptor.Cluster, descriptor.BackupID)
	if err != nil {
		return err
	}
	recoveryRoot := filepath.Join(m.saveRoot, filepath.FromSlash(receipt.RecoveryRef))
	targetShard := filepath.Join(roomPath, descriptor.Shard)
	recoveryShard := filepath.Join(recoveryRoot, "shard")
	if _, err := os.Lstat(recoveryShard); os.IsNotExist(err) {
		marker, _ := os.ReadFile(filepath.Join(targetShard, ".dst-admin-restore-id"))
		if strings.TrimSpace(string(marker)) == descriptor.BackupID {
			return ErrConflict
		}
		if err := os.Rename(targetShard, recoveryShard); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(targetShard); os.IsNotExist(err) {
		if err := os.Rename(filepath.Join(stage, "shard"), targetShard); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if marker, readErr := os.ReadFile(filepath.Join(targetShard, ".dst-admin-restore-id")); readErr != nil || strings.TrimSpace(string(marker)) != descriptor.BackupID {
		return ErrConflict
	}
	if receipt.PublishShared {
		incoming := make(map[string]bool, len(receipt.IncomingShared))
		for _, name := range receipt.IncomingShared {
			incoming[name] = true
		}
		for _, name := range sortedSharedNames() {
			target := filepath.Join(roomPath, name)
			if incoming[name] {
				if err := publishRegular(filepath.Join(stage, "shared", name), target); err != nil {
					return err
				}
			} else if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) validateRestoreStage(descriptor BackupDescriptor) error {
	stage, err := m.restoreStagePath(descriptor.Cluster, descriptor.BackupID)
	if err != nil {
		return err
	}
	if !regularExists(filepath.Join(stage, "shared", "cluster.ini")) || !regularExists(filepath.Join(stage, "shard", "server.ini")) {
		return ErrIntegrity
	}
	sharedSHA, err := hashSharedDirectory(filepath.Join(stage, "shared"))
	if err != nil || !strings.EqualFold(sharedSHA, descriptor.SharedSHA256) {
		return ErrIntegrity
	}
	if evidence := inspectSaveDirectory(filepath.Join(stage, "shard", "save")); !evidence.restorable() {
		return ErrIntegrity
	}
	return nil
}

func (m *Manager) restoreDescriptor(id string) (BackupDescriptor, error) {
	var value BackupDescriptor
	if err := readJSON(m.restoreMetaPath(id), &value); err != nil {
		return BackupDescriptor{}, err
	}
	if value.BackupID != id || validateBackupDescriptor(value) != nil ||
		(value.Phase != "receiving" && value.Phase != "prepared" && value.Phase != "published" && value.Phase != "completed") {
		return BackupDescriptor{}, ErrIntegrity
	}
	return value, nil
}

func (m *Manager) restoreReceipt(id string) (RestoreReceipt, error) {
	var value RestoreReceipt
	if err := readJSON(m.restoreReceiptPath(id), &value); err != nil {
		return RestoreReceipt{}, err
	}
	if value.BackupID != id || !migrationID.MatchString(id) || !resourceName.MatchString(value.Cluster) || !resourceName.MatchString(value.Shard) ||
		(value.Phase != "publishing" && value.Phase != "published" && value.Phase != "completing" && value.Phase != "completed") || value.RecoveryRef == "" {
		return RestoreReceipt{}, ErrIntegrity
	}
	return value, nil
}

func validateBackupDescriptor(value BackupDescriptor) error {
	if !migrationID.MatchString(value.BackupID) || !resourceName.MatchString(value.Cluster) || !resourceName.MatchString(value.Shard) ||
		value.Size < 1 || value.Size > maxTotalBytes || value.ContentSize < 1 || value.ContentSize > maxTotalBytes ||
		value.FileCount < 2 || value.FileCount > maxEntryCount || !validSHA256(value.SHA256) || !validSHA256(value.SharedSHA256) {
		return ErrInvalidRequest
	}
	return nil
}

func sameBackupDescriptor(left, right BackupDescriptor) bool {
	return left.BackupID == right.BackupID && left.Cluster == right.Cluster && left.Shard == right.Shard &&
		left.Size == right.Size && left.ContentSize == right.ContentSize && left.FileCount == right.FileCount &&
		strings.EqualFold(left.SHA256, right.SHA256) && strings.EqualFold(left.SharedSHA256, right.SharedSHA256)
}

// InspectBackupArchive verifies the bounded archive structure and classifies
// its restore capability without extracting it.
func InspectBackupArchive(path string) (BackupInspection, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return BackupInspection{}, ErrIntegrity
	}
	defer archive.Close()
	if len(archive.File) < 2 || len(archive.File) > maxEntryCount {
		return BackupInspection{}, ErrIntegrity
	}
	var contentSize int64
	shared := make(map[string][]byte)
	evidence := saveEvidence{}
	for _, entry := range archive.File {
		if entry.UncompressedSize64 > uint64(maxEntryBytes) || contentSize > maxTotalBytes-int64(entry.UncompressedSize64) {
			return BackupInspection{}, ErrIntegrity
		}
		contentSize += int64(entry.UncompressedSize64)
		name := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Name)))
		if name == "shard/save/shardindex" && entry.Mode().IsRegular() && entry.UncompressedSize64 > 0 {
			evidence.HasShardIndex = true
		}
		if sessionID, snapshot, ok := archiveSnapshotEvidence(name, entry); ok {
			evidence.addSnapshot(sessionID, snapshot)
		}
		if !strings.HasPrefix(name, "shared/") {
			continue
		}
		base := strings.TrimPrefix(name, "shared/")
		if !sharedFileNames[base] || strings.Contains(base, "/") {
			return BackupInspection{}, ErrIntegrity
		}
		reader, err := entry.Open()
		if err != nil {
			return BackupInspection{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxEntryBytes+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || int64(len(data)) != int64(entry.UncompressedSize64) {
			return BackupInspection{}, errors.Join(readErr, closeErr, ErrIntegrity)
		}
		shared[base] = data
	}
	return evidence.inspection(contentSize, len(archive.File), hashNamedData(shared)), nil
}

type saveEvidence struct {
	HasShardIndex  bool
	SessionID      string
	LatestSnapshot string
}

func (e saveEvidence) restorable() bool {
	return e.HasShardIndex && e.LatestSnapshot != ""
}

func (e *saveEvidence) addSnapshot(sessionID, snapshot string) {
	if e.LatestSnapshot == "" || snapshot > e.LatestSnapshot || snapshot == e.LatestSnapshot && sessionID < e.SessionID {
		e.SessionID, e.LatestSnapshot = sessionID, snapshot
	}
}

func (e saveEvidence) inspection(contentSize int64, fileCount int, sharedSHA string) BackupInspection {
	value := BackupInspection{
		ContentSize: contentSize, FileCount: fileCount, SharedSHA256: sharedSHA,
		ContentKind: BackupContentConfigurationOnly, SessionID: e.SessionID,
		LatestSnapshot: e.LatestSnapshot, HasShardIndex: e.HasShardIndex,
	}
	switch {
	case !e.HasShardIndex && e.LatestSnapshot == "":
		value.ValidationError = "备份不包含 save/shardindex 和 Session 快照"
	case !e.HasShardIndex:
		value.ValidationError = "备份不包含 save/shardindex"
	case e.LatestSnapshot == "":
		value.ValidationError = "备份不包含有效的 Session 快照"
	default:
		value.ContentKind, value.Restorable = BackupContentGameSave, true
	}
	return value
}

func archiveSnapshotEvidence(name string, entry *zip.File) (string, string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[0] != "shard" || parts[1] != "save" || parts[2] != "session" ||
		parts[3] == "" || !entry.Mode().IsRegular() || entry.UncompressedSize64 == 0 || !snapshotName(parts[4]) {
		return "", "", false
	}
	return parts[3], parts[4], true
}

func inspectSaveDirectory(saveRoot string) saveEvidence {
	value := saveEvidence{}
	if info, err := os.Lstat(filepath.Join(saveRoot, "shardindex")); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		value.HasShardIndex = true
	}
	sessions, err := os.ReadDir(filepath.Join(saveRoot, "session"))
	if err != nil {
		return value
	}
	for _, session := range sessions {
		if !session.IsDir() || session.Type()&os.ModeSymlink != 0 {
			continue
		}
		entries, readErr := os.ReadDir(filepath.Join(saveRoot, "session", session.Name()))
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			info, infoErr := entry.Info()
			if infoErr == nil && info.Mode().IsRegular() && info.Size() > 0 && snapshotName(entry.Name()) {
				value.addSnapshot(session.Name(), entry.Name())
			}
		}
	}
	return value
}

func snapshotName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func hashSharedDirectory(root string) (string, error) {
	values := make(map[string][]byte)
	for _, name := range sortedSharedNames() {
		path := filepath.Join(root, name)
		if !regularExists(path) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		values[name] = data
	}
	return hashNamedData(values), nil
}

func hashNamedData(values map[string][]byte) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00", name, len(values[name]))
		_, _ = hash.Write(values[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func publishRegular(source, target string) error {
	temporary, err := os.CreateTemp(filepath.Dir(target), ".dst-admin-publish-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	_ = temporary.Close()
	defer os.Remove(temporaryPath)
	if err := copyRegular(source, temporaryPath, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

func unionNames(groups ...[]string) []string {
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, name := range group {
			if sharedFileNames[name] {
				seen[name] = true
			}
		}
	}
	values := make([]string, 0, len(seen))
	for name := range seen {
		values = append(values, name)
	}
	sort.Strings(values)
	return values
}

func (m *Manager) restoreArchivePath(id string) string {
	return filepath.Join(m.stateRoot, "restore-"+id+".zip")
}

func (m *Manager) restoreMetaPath(id string) string {
	return filepath.Join(m.stateRoot, "restore-"+id+".json")
}

func (m *Manager) restoreStagePath(cluster, id string) (string, error) {
	return m.targetStagePath(cluster, "restore", id)
}

func (m *Manager) restoreReceiptPath(id string) string {
	return filepath.Join(m.stateRoot, "restore-receipt-"+id+".json")
}
