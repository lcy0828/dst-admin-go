package configpublication

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	MaxChunkBytes = 256 * 1024
	maxFileBytes  = int64(4 * 1024 * 1024)
	maxTotalBytes = int64(16 * 1024 * 1024)
)

var (
	ErrInvalidRequest = errors.New("configuration publication request is invalid")
	ErrConflict       = errors.New("configuration publication conflicts with current state")
	ErrIntegrity      = errors.New("configuration publication integrity check failed")
	identityPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	operationPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)
)

type Scope string

const (
	ScopeShared Scope = "shared"
	ScopeWorld  Scope = "world"
	ScopeMod    Scope = "mod"
)

type Descriptor struct {
	PublicationID string `json:"publicationId"`
	Cluster       string `json:"cluster"`
	Shard         string `json:"shard"`
	Scope         Scope  `json:"scope"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	Phase         string `json:"phase"`
}

type fileReceipt struct {
	Name    string      `json:"name"`
	Existed bool        `json:"existed"`
	Mode    os.FileMode `json:"mode"`
}

type receipt struct {
	PublicationID string        `json:"publicationId"`
	Cluster       string        `json:"cluster"`
	Shard         string        `json:"shard"`
	Scope         Scope         `json:"scope"`
	Phase         string        `json:"phase"`
	Files         []fileReceipt `json:"files"`
}

type Manager struct {
	saveRoot  string
	stateRoot string
}

func New(saveRoot, stateRoot string) (*Manager, error) {
	saveRoot = strings.TrimSpace(saveRoot)
	stateRoot = strings.TrimSpace(stateRoot)
	if saveRoot == "" || stateRoot == "" {
		return nil, ErrInvalidRequest
	}
	resolvedSave, err := filepath.Abs(saveRoot)
	if err != nil {
		return nil, err
	}
	resolvedState, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, err
	}
	if err := requireDirectory(resolvedSave); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(resolvedState, 0o700); err != nil {
		return nil, err
	}
	if err := requireDirectory(resolvedState); err != nil {
		return nil, err
	}
	return &Manager{saveRoot: filepath.Clean(resolvedSave), stateRoot: filepath.Clean(resolvedState)}, nil
}

func (m *Manager) Begin(descriptor Descriptor) (int64, error) {
	if err := validateDescriptor(descriptor); err != nil {
		return 0, err
	}
	if current, err := m.descriptor(descriptor.PublicationID); err == nil {
		if !sameDescriptor(current, descriptor) || current.Phase != "receiving" {
			return 0, ErrConflict
		}
		info, statErr := os.Stat(m.archivePath(descriptor.PublicationID))
		if statErr != nil {
			return 0, statErr
		}
		return info.Size(), nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	root := m.operationRoot(descriptor.PublicationID)
	if err := os.Mkdir(root, 0o700); err != nil {
		return 0, err
	}
	archive, err := os.OpenFile(m.archivePath(descriptor.PublicationID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(root)
		return 0, err
	}
	if err := archive.Close(); err != nil {
		_ = os.RemoveAll(root)
		return 0, err
	}
	descriptor.Phase = "receiving"
	if err := writeJSON(m.descriptorPath(descriptor.PublicationID), descriptor); err != nil {
		_ = os.RemoveAll(root)
		return 0, err
	}
	return 0, nil
}

func (m *Manager) Write(publicationID string, offset int64, data []byte) (int64, error) {
	if !operationPattern.MatchString(publicationID) || offset < 0 || len(data) == 0 || len(data) > MaxChunkBytes {
		return 0, ErrInvalidRequest
	}
	descriptor, err := m.descriptor(publicationID)
	if err != nil || descriptor.Phase != "receiving" || offset > descriptor.Size || int64(len(data)) > descriptor.Size-offset {
		return 0, errors.Join(err, ErrInvalidRequest)
	}
	archive, err := os.OpenFile(m.archivePath(publicationID), os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() != offset {
		return info.Size(), ErrConflict
	}
	written, err := archive.WriteAt(data, offset)
	if err != nil || written != len(data) {
		return offset + int64(written), errors.Join(err, io.ErrShortWrite)
	}
	if err := archive.Sync(); err != nil {
		return offset + int64(written), err
	}
	return offset + int64(written), nil
}

func (m *Manager) Prepare(ctx context.Context, publicationID string) error {
	descriptor, err := m.descriptor(publicationID)
	if err != nil {
		return err
	}
	if descriptor.Phase == "prepared" || descriptor.Phase == "published" {
		return nil
	}
	if descriptor.Phase != "receiving" {
		return ErrConflict
	}
	actualSize, checksum, err := describeFile(m.archivePath(publicationID))
	if err != nil || actualSize != descriptor.Size || !strings.EqualFold(checksum, descriptor.SHA256) {
		return errors.Join(err, ErrIntegrity)
	}
	stage := m.stagePath(publicationID)
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	if err := extractArchive(ctx, m.archivePath(publicationID), stage, descriptor.Scope); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	descriptor.Phase = "prepared"
	return writeJSON(m.descriptorPath(publicationID), descriptor)
}

func (m *Manager) Publish(publicationID string) error {
	return m.publish(publicationID, nil)
}

func (m *Manager) publish(publicationID string, expected map[string]string) error {
	descriptor, err := m.descriptor(publicationID)
	if err != nil {
		return err
	}
	if descriptor.Phase == "published" {
		return nil
	}
	if descriptor.Phase != "prepared" {
		return ErrConflict
	}
	target, err := m.targetRoot(descriptor)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(m.stagePath(publicationID))
	if err != nil || len(entries) == 0 {
		return errors.Join(err, ErrIntegrity)
	}
	if expected != nil && len(entries) != len(expected) {
		return ErrInvalidRequest
	}
	recovery := m.recoveryPath(publicationID)
	if err := os.Mkdir(recovery, 0o700); err != nil {
		return err
	}
	value := receipt{PublicationID: publicationID, Cluster: descriptor.Cluster, Shard: descriptor.Shard, Scope: descriptor.Scope, Phase: "publishing"}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !allowedName(descriptor.Scope, name) {
			return ErrIntegrity
		}
		targetPath := filepath.Join(target, name)
		if expected != nil {
			digest, exists := expected[name]
			if !exists {
				return ErrInvalidRequest
			}
			if err := verifyExpectedFile(targetPath, digest); err != nil {
				return err
			}
		}
		info, statErr := os.Lstat(targetPath)
		item := fileReceipt{Name: name, Mode: 0o640}
		if statErr == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxFileBytes {
				return ErrConflict
			}
			item.Existed, item.Mode = true, info.Mode().Perm()
			if err := copyRegular(targetPath, filepath.Join(recovery, name), 0o600); err != nil {
				return err
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		value.Files = append(value.Files, item)
	}
	sort.Slice(value.Files, func(i, j int) bool { return value.Files[i].Name < value.Files[j].Name })
	if err := writeJSON(m.receiptPath(publicationID), value); err != nil {
		return err
	}
	for _, item := range value.Files {
		data, readErr := os.ReadFile(filepath.Join(m.stagePath(publicationID), item.Name))
		if readErr != nil {
			_ = m.restore(value)
			return readErr
		}
		if writeErr := atomicWrite(filepath.Join(target, item.Name), data, item.Mode); writeErr != nil {
			rollbackErr := m.restore(value)
			return errors.Join(writeErr, rollbackErr)
		}
	}
	value.Phase = "published"
	if err := writeJSON(m.receiptPath(publicationID), value); err != nil {
		rollbackErr := m.restore(value)
		return errors.Join(err, rollbackErr)
	}
	descriptor.Phase = "published"
	if err := writeJSON(m.descriptorPath(publicationID), descriptor); err != nil {
		rollbackErr := m.restore(value)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func (m *Manager) Rollback(publicationID string) error {
	if !operationPattern.MatchString(publicationID) {
		return ErrInvalidRequest
	}
	value, err := m.receipt(publicationID)
	if err == nil {
		if restoreErr := m.restore(value); restoreErr != nil {
			return restoreErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(m.operationRoot(publicationID))
}

func (m *Manager) Complete(publicationID string) error {
	descriptor, err := m.descriptor(publicationID)
	if err != nil {
		return err
	}
	if descriptor.Phase != "published" {
		return ErrConflict
	}
	return os.RemoveAll(m.operationRoot(publicationID))
}

func (m *Manager) restore(value receipt) error {
	descriptor, err := m.descriptor(value.PublicationID)
	if err != nil {
		return err
	}
	target, err := m.targetRoot(descriptor)
	if err != nil {
		return err
	}
	var result error
	for index := len(value.Files) - 1; index >= 0; index-- {
		item := value.Files[index]
		path := filepath.Join(target, item.Name)
		if item.Existed {
			data, readErr := os.ReadFile(filepath.Join(m.recoveryPath(value.PublicationID), item.Name))
			if readErr != nil {
				result = errors.Join(result, readErr)
				continue
			}
			result = errors.Join(result, atomicWrite(path, data, item.Mode))
		} else if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			result = errors.Join(result, removeErr)
		}
	}
	return result
}

func (m *Manager) targetRoot(descriptor Descriptor) (string, error) {
	root := filepath.Join(m.saveRoot, descriptor.Cluster)
	if descriptor.Scope == ScopeWorld || descriptor.Scope == ScopeMod {
		root = filepath.Join(root, descriptor.Shard)
	}
	if !contained(m.saveRoot, root) {
		return "", ErrInvalidRequest
	}
	if err := requireDirectory(root); err != nil {
		return "", err
	}
	return root, nil
}

func (m *Manager) descriptor(id string) (Descriptor, error) {
	var value Descriptor
	err := readJSON(m.descriptorPath(id), &value)
	return value, err
}

func (m *Manager) receipt(id string) (receipt, error) {
	var value receipt
	err := readJSON(m.receiptPath(id), &value)
	return value, err
}

func (m *Manager) operationRoot(id string) string { return filepath.Join(m.stateRoot, id) }
func (m *Manager) archivePath(id string) string {
	return filepath.Join(m.operationRoot(id), "payload.zip")
}
func (m *Manager) descriptorPath(id string) string {
	return filepath.Join(m.operationRoot(id), "descriptor.json")
}
func (m *Manager) receiptPath(id string) string {
	return filepath.Join(m.operationRoot(id), "receipt.json")
}
func (m *Manager) stagePath(id string) string { return filepath.Join(m.operationRoot(id), "stage") }
func (m *Manager) recoveryPath(id string) string {
	return filepath.Join(m.operationRoot(id), "recovery")
}

func validateDescriptor(value Descriptor) error {
	if !operationPattern.MatchString(value.PublicationID) || !identityPattern.MatchString(value.Cluster) ||
		!identityPattern.MatchString(value.Shard) || value.Scope != ScopeShared && value.Scope != ScopeWorld && value.Scope != ScopeMod ||
		value.Size < 1 || value.Size > maxTotalBytes || len(value.SHA256) != 64 {
		return ErrInvalidRequest
	}
	if _, err := hex.DecodeString(value.SHA256); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func sameDescriptor(left, right Descriptor) bool {
	return left.PublicationID == right.PublicationID && left.Cluster == right.Cluster && left.Shard == right.Shard &&
		left.Scope == right.Scope && left.Size == right.Size && strings.EqualFold(left.SHA256, right.SHA256)
}

func allowedName(scope Scope, name string) bool {
	if filepath.Base(name) != name || name == "." || strings.ContainsAny(name, "/\\\x00\r\n") {
		return false
	}
	if scope == ScopeShared {
		switch name {
		case "cluster.ini", "cluster_token.txt", "adminlist.txt", "blocklist.txt", "whitelist.txt":
			return true
		}
		return false
	}
	if scope == ScopeWorld {
		return name == "server.ini" || name == "leveldataoverride.lua"
	}
	return scope == ScopeMod && name == "modoverrides.lua"
}

func extractArchive(ctx context.Context, archivePath, stage string, scope Scope) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errors.Join(err, ErrIntegrity)
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > 5 {
		return ErrIntegrity
	}
	seen := make(map[string]bool, len(reader.File))
	var total int64
	for _, item := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := item.Name
		if !allowedName(scope, name) || seen[name] || item.FileInfo().IsDir() || item.Mode()&os.ModeSymlink != 0 || item.UncompressedSize64 > uint64(maxFileBytes) {
			return ErrIntegrity
		}
		seen[name] = true
		if total > maxTotalBytes-int64(item.UncompressedSize64) {
			return ErrIntegrity
		}
		total += int64(item.UncompressedSize64)
		input, err := item.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(filepath.Join(stage, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, maxFileBytes+1))
		closeErr := errors.Join(input.Close(), output.Sync(), output.Close())
		if copyErr != nil || closeErr != nil || written != int64(item.UncompressedSize64) || written > maxFileBytes {
			return errors.Join(copyErr, closeErr, ErrIntegrity)
		}
	}
	return nil
}

func describeFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxTotalBytes+1))
	if err != nil || size > maxTotalBytes {
		return size, "", errors.Join(err, ErrIntegrity)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o600)
}

func readJSON(path string, target interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".config-publication-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func copyRegular(source, target string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, io.LimitReader(input, maxFileBytes+1))
	closeErr := errors.Join(output.Sync(), output.Close())
	return errors.Join(copyErr, closeErr)
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidRequest
	}
	return nil
}

func contained(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func (d Descriptor) String() string {
	return fmt.Sprintf("%s/%s/%s", d.Cluster, d.Shard, d.Scope)
}
