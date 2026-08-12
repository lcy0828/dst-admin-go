package dstruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	backupDirectory     = ".dst-admin-backups"
	backupSchemaVersion = 1
	backupManifestName  = "manifest.json"
)

var backupIDPattern = regexp.MustCompile(`^state-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{32}$`)

type backupManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	ID            string       `json:"id"`
	CreatedAt     time.Time    `json:"createdAt"`
	Reason        string       `json:"reason"`
	Files         []backupFile `json:"files"`
}

type backupFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

func (m *Manager) createStateBackup(worldPath, reason string) (string, error) {
	files, err := captureFiles(worldPath, runtimeStatePaths())
	if err != nil {
		return "", err
	}
	createdAt := m.now().UTC()
	id := fmt.Sprintf("state-%s-%s", createdAt.Format("20060102T150405.000000000Z"), strings.ReplaceAll(uuid.NewString(), "-", ""))
	root := filepath.Join(worldPath, backupDirectory)
	if err := os.MkdirAll(root, 0750); err != nil {
		return "", fmt.Errorf("create runtime backup root: %w", err)
	}
	temporary, err := os.MkdirTemp(root, ".state-")
	if err != nil {
		return "", fmt.Errorf("create runtime backup staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	manifest := backupManifest{SchemaVersion: backupSchemaVersion, ID: id, CreatedAt: createdAt, Reason: reason, Files: make([]backupFile, 0, len(files))}
	for _, file := range files {
		entry := backupFile{Path: file.path, Exists: file.exists, Mode: uint32(file.mode.Perm())}
		if file.exists {
			digest := sha256.Sum256(file.data)
			entry.SHA256 = hex.EncodeToString(digest[:])
			if err := atomicWrite(filepath.Join(temporary, "files", file.path), file.data, file.mode); err != nil {
				return "", fmt.Errorf("write runtime backup %s: %w", file.path, err)
			}
		}
		manifest.Files = append(manifest.Files, entry)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(temporary, backupManifestName), append(encoded, '\n'), 0600); err != nil {
		return "", err
	}
	final := filepath.Join(root, id)
	if err := os.Rename(temporary, final); err != nil {
		return "", fmt.Errorf("publish runtime backup: %w", err)
	}
	return id, nil
}

func (m *Manager) restoreStateBackup(worldPath, backupID string) error {
	manifest, files, err := readStateBackup(worldPath, backupID)
	if err != nil {
		return err
	}
	_ = manifest
	current, err := captureFiles(worldPath, runtimeStatePaths())
	if err != nil {
		return err
	}
	if err := validateCapturedLua(files); err != nil {
		return err
	}
	if err := restoreFiles(worldPath, files); err != nil {
		return errors.Join(err, restoreFiles(worldPath, current))
	}
	removeEmptyManagedDirectory(worldPath)
	return nil
}

func readStateBackup(worldPath, backupID string) (backupManifest, []capturedFile, error) {
	backupID = strings.TrimSpace(backupID)
	if !backupIDPattern.MatchString(backupID) {
		return backupManifest{}, nil, ErrUnsafeRuntimePath
	}
	root := filepath.Join(worldPath, backupDirectory, backupID)
	data, _, exists, err := readRegular(filepath.Join(root, backupManifestName), 64*1024)
	if err != nil || !exists {
		if err == nil {
			err = os.ErrNotExist
		}
		return backupManifest{}, nil, fmt.Errorf("read runtime backup manifest: %w", err)
	}
	var manifest backupManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return backupManifest{}, nil, fmt.Errorf("decode runtime backup manifest: %w", err)
	}
	if manifest.SchemaVersion != backupSchemaVersion || manifest.ID != backupID || manifest.CreatedAt.IsZero() || len(manifest.Files) != len(runtimeStatePaths()) {
		return backupManifest{}, nil, errors.New("runtime backup manifest is invalid")
	}
	allowed := make(map[string]bool, len(runtimeStatePaths()))
	for _, name := range runtimeStatePaths() {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(manifest.Files))
	files := make([]capturedFile, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		if !allowed[entry.Path] || seen[entry.Path] {
			return backupManifest{}, nil, errors.New("runtime backup contains an unsafe file path")
		}
		seen[entry.Path] = true
		file := capturedFile{path: entry.Path, exists: entry.Exists, mode: os.FileMode(entry.Mode).Perm()}
		if entry.Exists {
			value, mode, exists, readErr := readRegular(filepath.Join(root, "files", entry.Path), maxManagedBytes)
			if readErr != nil || !exists {
				return backupManifest{}, nil, errors.New("runtime backup payload is missing")
			}
			digest := sha256.Sum256(value)
			if entry.SHA256 == "" || hex.EncodeToString(digest[:]) != entry.SHA256 {
				return backupManifest{}, nil, errors.New("runtime backup payload checksum does not match")
			}
			file.data = value
			if file.mode == 0 {
				file.mode = mode
			}
		} else if entry.SHA256 != "" {
			return backupManifest{}, nil, errors.New("runtime backup manifest has an invalid missing file")
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return manifest, files, nil
}

func runtimeStatePaths() []string {
	return append([]string{customCommandsName}, managedPaths()...)
}

func removeEmptyManagedDirectory(worldPath string) {
	managedRoot := filepath.Join(worldPath, managedDirectory)
	entries, err := os.ReadDir(managedRoot)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(managedRoot)
	}
}
