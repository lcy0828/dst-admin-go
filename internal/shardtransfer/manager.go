package shardtransfer

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	MaxChunkBytes = 256 * 1024
	maxEntryBytes = int64(16 * 1024 * 1024 * 1024)
	maxTotalBytes = int64(64 * 1024 * 1024 * 1024)
	maxEntryCount = 100000
)

const (
	MaximumTransferBytes   = maxTotalBytes
	MaximumTransferEntries = maxEntryCount
)

var (
	ErrInvalidRequest = errors.New("shard transfer request is invalid")
	ErrConflict       = errors.New("shard transfer target conflicts with existing data")
	ErrIntegrity      = errors.New("shard transfer integrity check failed")
	resourceName      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	migrationID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
)

var sharedFileNames = map[string]bool{
	"cluster.ini": true, "cluster_token.txt": true, "adminlist.txt": true,
	"blocklist.txt": true, "whitelist.txt": true,
}

type Descriptor struct {
	MigrationID string `json:"migrationId"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type Chunk struct {
	Offset     int64
	NextOffset int64
	Size       int64
	SHA256     string
	Data       []byte
	Complete   bool
}

type receipt struct {
	MigrationID   string   `json:"migrationId"`
	Cluster       string   `json:"cluster"`
	Shard         string   `json:"shard"`
	CreatedShared []string `json:"createdShared,omitempty"`
	RecoveryRef   string   `json:"recoveryRef,omitempty"`
}

type Manager struct {
	saveRoot  string
	stateRoot string
}

func New(saveRoot, stateRoot string) (*Manager, error) {
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, ErrInvalidRequest
	}
	state, err := filepath.Abs(strings.TrimSpace(stateRoot))
	if err != nil || strings.TrimSpace(stateRoot) == "" {
		return nil, ErrInvalidRequest
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	return &Manager{saveRoot: filepath.Clean(root), stateRoot: filepath.Clean(state)}, nil
}

func (m *Manager) PrepareExport(ctx context.Context, id, cluster, shard string) (Descriptor, error) {
	if err := validateIdentity(id, cluster, shard); err != nil {
		return Descriptor{}, err
	}
	if existing, err := m.exportDescriptor(id); err == nil {
		return existing, nil
	}
	clusterPath, shardPath, err := m.sourcePaths(cluster, shard)
	if err != nil {
		return Descriptor{}, err
	}
	temporary, err := os.CreateTemp(m.stateRoot, ".export-*.tmp")
	if err != nil {
		return Descriptor{}, err
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	archive := zip.NewWriter(temporary)
	for _, name := range sortedSharedNames() {
		path := filepath.Join(clusterPath, name)
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			continue
		}
		if err := addRegularFile(ctx, archive, path, "shared/"+name); err != nil {
			_ = archive.Close()
			return Descriptor{}, err
		}
	}
	entries, total := 0, int64(0)
	err = filepath.Walk(shardPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(shardPath, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return ErrIntegrity
		}
		if relative == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return ErrIntegrity
		}
		entries++
		if entries > maxEntryCount {
			return ErrIntegrity
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() < 0 || info.Size() > maxEntryBytes || total > maxTotalBytes-info.Size() {
			return ErrIntegrity
		}
		total += info.Size()
		return addRegularFile(ctx, archive, path, filepath.ToSlash(filepath.Join("shard", relative)))
	})
	if err != nil {
		_ = archive.Close()
		return Descriptor{}, err
	}
	if err := archive.Close(); err != nil {
		return Descriptor{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Descriptor{}, err
	}
	if err := temporary.Close(); err != nil {
		return Descriptor{}, err
	}
	descriptor, err := describeFile(id, temporaryPath)
	if err != nil {
		return Descriptor{}, err
	}
	finalPath := m.exportPath(id)
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Descriptor{}, err
	}
	if err := writeJSON(m.exportMetaPath(id), descriptor); err != nil {
		_ = os.Remove(finalPath)
		return Descriptor{}, err
	}
	published = true
	return descriptor, nil
}

func (m *Manager) ReadExport(ctx context.Context, id string, offset int64) (Chunk, error) {
	if !migrationID.MatchString(id) || offset < 0 {
		return Chunk{}, ErrInvalidRequest
	}
	descriptor, err := m.exportDescriptor(id)
	if err != nil || offset > descriptor.Size {
		return Chunk{}, ErrInvalidRequest
	}
	file, err := os.Open(m.exportPath(id))
	if err != nil {
		return Chunk{}, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return Chunk{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxChunkBytes))
	if err != nil {
		return Chunk{}, err
	}
	if err := ctx.Err(); err != nil {
		return Chunk{}, err
	}
	next := offset + int64(len(data))
	return Chunk{Offset: offset, NextOffset: next, Size: descriptor.Size, SHA256: descriptor.SHA256, Data: data, Complete: next == descriptor.Size}, nil
}

func (m *Manager) ReleaseExport(id string) error {
	if !migrationID.MatchString(id) {
		return ErrInvalidRequest
	}
	return errors.Join(removeIfExists(m.exportPath(id)), removeIfExists(m.exportMetaPath(id)))
}

func (m *Manager) BeginImport(id string, size int64, checksum string) (Descriptor, error) {
	if !migrationID.MatchString(id) || size < 1 || size > maxTotalBytes || !validSHA256(checksum) {
		return Descriptor{}, ErrInvalidRequest
	}
	descriptor := Descriptor{MigrationID: id, Size: size, SHA256: strings.ToLower(checksum)}
	file, err := os.OpenFile(m.importPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Descriptor{}, err
	}
	if err := file.Close(); err != nil {
		return Descriptor{}, err
	}
	if err := writeJSON(m.importMetaPath(id), descriptor); err != nil {
		_ = os.Remove(m.importPath(id))
		return Descriptor{}, err
	}
	return descriptor, nil
}

func (m *Manager) WriteImport(id string, offset int64, data []byte) (int64, error) {
	if !migrationID.MatchString(id) || offset < 0 || len(data) < 1 || len(data) > MaxChunkBytes {
		return 0, ErrInvalidRequest
	}
	descriptor, err := m.importDescriptor(id)
	if err != nil || offset > descriptor.Size || int64(len(data)) > descriptor.Size-offset {
		return 0, ErrInvalidRequest
	}
	file, err := os.OpenFile(m.importPath(id), os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != offset {
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

func (m *Manager) CommitImport(ctx context.Context, id, cluster, shard string) (Descriptor, error) {
	if err := validateIdentity(id, cluster, shard); err != nil {
		return Descriptor{}, err
	}
	descriptor, err := m.importDescriptor(id)
	if err != nil {
		return Descriptor{}, err
	}
	actual, err := describeFile(id, m.importPath(id))
	if err != nil || actual.Size != descriptor.Size || actual.SHA256 != descriptor.SHA256 {
		return Descriptor{}, ErrIntegrity
	}
	roomPath, err := m.targetRoomPath(cluster)
	if err != nil {
		return Descriptor{}, err
	}
	targetShard := filepath.Join(roomPath, shard)
	if _, err := os.Lstat(targetShard); err == nil {
		return Descriptor{}, ErrConflict
	} else if !os.IsNotExist(err) {
		return Descriptor{}, err
	}
	staging, err := os.MkdirTemp(m.stateRoot, ".extract-")
	if err != nil {
		return Descriptor{}, err
	}
	defer os.RemoveAll(staging)
	if err := extractArchive(ctx, m.importPath(id), staging); err != nil {
		return Descriptor{}, err
	}
	if !regularExists(filepath.Join(staging, "shared", "cluster.ini")) || !regularExists(filepath.Join(staging, "shard", "server.ini")) {
		return Descriptor{}, ErrIntegrity
	}
	if err := os.MkdirAll(roomPath, 0o750); err != nil {
		return Descriptor{}, err
	}
	createdShared := make([]string, 0)
	for _, name := range sortedSharedNames() {
		source := filepath.Join(staging, "shared", name)
		if !regularExists(source) {
			continue
		}
		target := filepath.Join(roomPath, name)
		if existing, readErr := os.ReadFile(target); readErr == nil {
			incoming, incomingErr := os.ReadFile(source)
			if incomingErr != nil || !bytesEqual(existing, incoming) {
				return Descriptor{}, ErrConflict
			}
			continue
		} else if !os.IsNotExist(readErr) {
			return Descriptor{}, readErr
		}
		if err := copyRegular(source, target, 0o600); err != nil {
			return Descriptor{}, err
		}
		createdShared = append(createdShared, name)
	}
	marker := filepath.Join(staging, "shard", ".dst-admin-migration-id")
	if err := os.WriteFile(marker, []byte(id+"\n"), 0o600); err != nil {
		return Descriptor{}, err
	}
	if err := os.Rename(filepath.Join(staging, "shard"), targetShard); err != nil {
		for _, name := range createdShared {
			_ = os.Remove(filepath.Join(roomPath, name))
		}
		return Descriptor{}, err
	}
	if err := writeJSON(m.targetReceiptPath(id), receipt{MigrationID: id, Cluster: cluster, Shard: shard, CreatedShared: createdShared}); err != nil {
		_ = os.RemoveAll(targetShard)
		for _, name := range createdShared {
			_ = os.Remove(filepath.Join(roomPath, name))
		}
		return Descriptor{}, err
	}
	return descriptor, nil
}

func (m *Manager) RollbackTarget(id string) error {
	value, err := m.readReceipt(m.targetReceiptPath(id))
	if os.IsNotExist(err) {
		return errors.Join(removeIfExists(m.importPath(id)), removeIfExists(m.importMetaPath(id)))
	}
	if err != nil {
		return err
	}
	roomPath, err := m.targetRoomPath(value.Cluster)
	if err != nil {
		return err
	}
	shardPath := filepath.Join(roomPath, value.Shard)
	marker, err := os.ReadFile(filepath.Join(shardPath, ".dst-admin-migration-id"))
	if err != nil || strings.TrimSpace(string(marker)) != id {
		return ErrConflict
	}
	if err := os.RemoveAll(shardPath); err != nil {
		return err
	}
	for _, name := range value.CreatedShared {
		_ = os.Remove(filepath.Join(roomPath, name))
	}
	return errors.Join(removeIfExists(m.targetReceiptPath(id)), removeIfExists(m.importPath(id)), removeIfExists(m.importMetaPath(id)))
}

func (m *Manager) CompleteTarget(id string) error {
	value, err := m.readReceipt(m.targetReceiptPath(id))
	if err != nil {
		return err
	}
	roomPath, err := m.targetRoomPath(value.Cluster)
	if err != nil {
		return err
	}
	marker := filepath.Join(roomPath, value.Shard, ".dst-admin-migration-id")
	data, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(data)) != id {
		return ErrConflict
	}
	return errors.Join(os.Remove(marker), removeIfExists(m.targetReceiptPath(id)), removeIfExists(m.importPath(id)), removeIfExists(m.importMetaPath(id)))
}

func (m *Manager) FinalizeSource(id, cluster, shard string) (string, error) {
	if err := validateIdentity(id, cluster, shard); err != nil {
		return "", err
	}
	if existing, err := m.readReceipt(m.sourceReceiptPath(id)); err == nil {
		if existing.Cluster != cluster || existing.Shard != shard || strings.TrimSpace(existing.RecoveryRef) == "" {
			return "", ErrConflict
		}
		if _, statErr := os.Lstat(filepath.Join(m.saveRoot, filepath.FromSlash(existing.RecoveryRef))); statErr != nil {
			return "", statErr
		}
		return existing.RecoveryRef, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	clusterPath, err := m.targetRoomPath(cluster)
	if err != nil {
		return "", err
	}
	shardPath := filepath.Join(clusterPath, shard)
	recoveryRoot := filepath.Join(clusterPath, ".dst-admin-migrations")
	if err := os.MkdirAll(recoveryRoot, 0o700); err != nil {
		return "", err
	}
	recoveryName := id + "-" + shard
	recoveryPath := filepath.Join(recoveryRoot, recoveryName)
	if _, err := os.Lstat(recoveryPath); err == nil {
		if _, activeErr := os.Lstat(shardPath); activeErr == nil {
			return "", ErrConflict
		} else if !os.IsNotExist(activeErr) {
			return "", activeErr
		}
		recoveryRef := filepath.ToSlash(filepath.Join(cluster, ".dst-admin-migrations", recoveryName))
		if err := writeJSON(m.sourceReceiptPath(id), receipt{MigrationID: id, Cluster: cluster, Shard: shard, RecoveryRef: recoveryRef}); err != nil {
			return "", err
		}
		return recoveryRef, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	info, err := os.Lstat(shardPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrInvalidRequest
	}
	if err := os.Rename(shardPath, recoveryPath); err != nil {
		return "", err
	}
	recoveryRef := filepath.ToSlash(filepath.Join(cluster, ".dst-admin-migrations", recoveryName))
	if err := writeJSON(m.sourceReceiptPath(id), receipt{MigrationID: id, Cluster: cluster, Shard: shard, RecoveryRef: recoveryRef}); err != nil {
		_ = os.Rename(recoveryPath, shardPath)
		return "", err
	}
	return recoveryRef, nil
}

func (m *Manager) RollbackSource(id string) error {
	value, err := m.readReceipt(m.sourceReceiptPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	clusterPath, err := m.targetRoomPath(value.Cluster)
	if err != nil {
		return err
	}
	recoveryPath := filepath.Join(m.saveRoot, filepath.FromSlash(value.RecoveryRef))
	target := filepath.Join(clusterPath, value.Shard)
	if _, err := os.Lstat(target); err == nil {
		return ErrConflict
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(recoveryPath, target); err != nil {
		return err
	}
	return removeIfExists(m.sourceReceiptPath(id))
}

func (m *Manager) CompleteSource(id string) (string, error) {
	value, err := m.readReceipt(m.sourceReceiptPath(id))
	if err != nil {
		return "", err
	}
	if err := os.Remove(m.sourceReceiptPath(id)); err != nil {
		return "", err
	}
	return value.RecoveryRef, nil
}

func (m *Manager) sourcePaths(cluster, shard string) (string, string, error) {
	root, err := filepath.EvalSymlinks(m.saveRoot)
	if err != nil {
		return "", "", err
	}
	clusterPath, err := filepath.EvalSymlinks(filepath.Join(root, cluster))
	if err != nil || !contained(root, clusterPath) {
		return "", "", ErrInvalidRequest
	}
	shardPath, err := filepath.EvalSymlinks(filepath.Join(clusterPath, shard))
	if err != nil || !contained(clusterPath, shardPath) {
		return "", "", ErrInvalidRequest
	}
	return clusterPath, shardPath, nil
}

func (m *Manager) targetRoomPath(cluster string) (string, error) {
	if !resourceName.MatchString(cluster) {
		return "", ErrInvalidRequest
	}
	root, err := filepath.EvalSymlinks(m.saveRoot)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(root, cluster)
	if !contained(root, candidate) {
		return "", ErrInvalidRequest
	}
	return candidate, nil
}

func (m *Manager) exportPath(id string) string {
	return filepath.Join(m.stateRoot, "export-"+id+".zip")
}
func (m *Manager) exportMetaPath(id string) string {
	return filepath.Join(m.stateRoot, "export-"+id+".json")
}
func (m *Manager) importPath(id string) string {
	return filepath.Join(m.stateRoot, "import-"+id+".zip")
}
func (m *Manager) importMetaPath(id string) string {
	return filepath.Join(m.stateRoot, "import-"+id+".json")
}
func (m *Manager) targetReceiptPath(id string) string {
	return filepath.Join(m.stateRoot, "target-"+id+".json")
}
func (m *Manager) sourceReceiptPath(id string) string {
	return filepath.Join(m.stateRoot, "source-"+id+".json")
}

func (m *Manager) exportDescriptor(id string) (Descriptor, error) {
	return readDescriptor(m.exportMetaPath(id), id)
}
func (m *Manager) importDescriptor(id string) (Descriptor, error) {
	return readDescriptor(m.importMetaPath(id), id)
}

func (m *Manager) readReceipt(path string) (receipt, error) {
	var value receipt
	err := readJSON(path, &value)
	if err == nil && (!migrationID.MatchString(value.MigrationID) || !resourceName.MatchString(value.Cluster) || !resourceName.MatchString(value.Shard)) {
		err = ErrIntegrity
	}
	return value, err
}

func validateIdentity(id, cluster, shard string) error {
	if !migrationID.MatchString(id) || !resourceName.MatchString(cluster) || !resourceName.MatchString(shard) {
		return ErrInvalidRequest
	}
	return nil
}

func addRegularFile(ctx context.Context, archive *zip.Writer, path, name string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxEntryBytes {
		return ErrIntegrity
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name, header.Method = filepath.ToSlash(name), zip.Deflate
	header.SetMode(0o600)
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = copyContext(ctx, writer, file)
	return err
}

func extractArchive(ctx context.Context, path, staging string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return ErrIntegrity
	}
	defer archive.Close()
	if len(archive.File) < 2 || len(archive.File) > maxEntryCount {
		return ErrIntegrity
	}
	var total int64
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.Clean(filepath.FromSlash(entry.Name))
		if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) ||
			!(strings.HasPrefix(filepath.ToSlash(clean), "shared/") || strings.HasPrefix(filepath.ToSlash(clean), "shard/")) ||
			entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() || entry.UncompressedSize64 > uint64(maxEntryBytes) {
			return ErrIntegrity
		}
		total += int64(entry.UncompressedSize64)
		if total > maxTotalBytes {
			return ErrIntegrity
		}
		target := filepath.Join(staging, clean)
		if !contained(staging, target) {
			return ErrIntegrity
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = reader.Close()
			return err
		}
		written, copyErr := copyContext(ctx, file, io.LimitReader(reader, int64(entry.UncompressedSize64)+1))
		closeErr := errors.Join(file.Close(), reader.Close())
		if copyErr != nil || closeErr != nil || written != int64(entry.UncompressedSize64) {
			return errors.Join(copyErr, closeErr, ErrIntegrity)
		}
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil || count != read {
				return written, errors.Join(writeErr, io.ErrShortWrite)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func describeFile(id, path string) (Descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return Descriptor{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return Descriptor{}, err
	}
	return Descriptor{MigrationID: id, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func readDescriptor(path, id string) (Descriptor, error) {
	var value Descriptor
	if err := readJSON(path, &value); err != nil {
		return Descriptor{}, err
	}
	if value.MigrationID != id || value.Size < 1 || value.Size > maxTotalBytes || !validSHA256(value.SHA256) {
		return Descriptor{}, ErrIntegrity
	}
	return value, nil
}

func writeJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readJSON(path string, destination interface{}) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return ErrIntegrity
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func copyRegular(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, mode)
}

func regularExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func sortedSharedNames() []string {
	values := make([]string, 0, len(sharedFileNames))
	for name := range sharedFileNames {
		values = append(values, name)
	}
	sort.Strings(values)
	return values
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func contained(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
