package maptransfer

import (
	"archive/zip"
	"bytes"
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

	"dont/internal/maprenderer"
	"dont/internal/worldmap"
	"dont/shared"
)

const (
	MaxSessions        = 500
	MaximumTransfer    = int64(256 * 1024 * 1024)
	maximumSnapshot    = int64(128 * 1024 * 1024)
	maximumRendererLog = 128 * 1024
	stagingRetention   = 24 * time.Hour
)

var (
	ErrInvalidRequest  = errors.New("map transfer request is invalid")
	ErrTransferMissing = errors.New("map transfer is missing")
)

type Renderer interface {
	Available() (bool, string)
	Render(context.Context, string, string, []worldmap.Layer, io.Writer) error
}

type Descriptor struct {
	TransferID   string `json:"transferId"`
	Kind         string `json:"kind"`
	Cluster      string `json:"cluster"`
	Shard        string `json:"shard"`
	SessionID    string `json:"sessionId"`
	FileName     string `json:"fileName"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	SourceSHA256 string `json:"sourceSha256"`
	Log          string `json:"log,omitempty"`
}

type Chunk struct {
	Descriptor
	Offset     int64
	NextOffset int64
	Data       []byte
	Complete   bool
}

type Manager struct {
	saveRoot  string
	stateRoot string
	renderer  Renderer
}

func New(saveRoot, stateRoot string, renderer Renderer) (*Manager, error) {
	if renderer == nil {
		return nil, ErrInvalidRequest
	}
	resolvedSave, err := trustedRoot(saveRoot)
	if err != nil {
		return nil, err
	}
	resolvedState, err := filepath.Abs(strings.TrimSpace(stateRoot))
	if err != nil || strings.TrimSpace(stateRoot) == "" {
		return nil, ErrInvalidRequest
	}
	resolvedState = filepath.Clean(resolvedState)
	if err := os.MkdirAll(resolvedState, 0o700); err != nil {
		return nil, err
	}
	manager := &Manager{saveRoot: resolvedSave, stateRoot: resolvedState, renderer: renderer}
	if err := manager.cleanupExpired(time.Now()); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Sessions(cluster, shard string) ([]shared.RuntimeMapSession, error) {
	root, err := m.sessionRoot(cluster, shard)
	if errors.Is(err, os.ErrNotExist) {
		return []shared.RuntimeMapSession{}, nil
	}
	if err != nil {
		return nil, err
	}
	directories, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	result := make([]shared.RuntimeMapSession, 0)
	for _, directory := range directories {
		if !directory.IsDir() || !safeComponent(directory.Name()) {
			continue
		}
		directoryPath, err := safeDirectory(root, directory.Name())
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(directoryPath)
		if err != nil {
			continue
		}
		players := 0
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "KU_") {
				players++
			}
		}
		for _, entry := range entries {
			if entry.IsDir() || !safeComponent(entry.Name()) || strings.HasPrefix(entry.Name(), ".") || strings.HasSuffix(strings.ToLower(entry.Name()), ".meta") {
				continue
			}
			info, err := os.Lstat(filepath.Join(directoryPath, entry.Name()))
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximumSnapshot {
				continue
			}
			result = append(result, shared.RuntimeMapSession{
				SessionID: directory.Name(), FileName: entry.Name(), Size: info.Size(),
				PlayerCount: players, ModifiedAt: info.ModTime().UTC(),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ModifiedAt.Equal(result[j].ModifiedAt) {
			if result[i].SessionID == result[j].SessionID {
				return result[i].FileName > result[j].FileName
			}
			return result[i].SessionID > result[j].SessionID
		}
		return result[i].ModifiedAt.After(result[j].ModifiedAt)
	})
	if len(result) > MaxSessions {
		result = result[:MaxSessions]
	}
	return result, nil
}

func (m *Manager) RendererStatus() shared.RuntimeMapRenderer {
	if provider, ok := m.renderer.(interface{ Info() worldmap.RendererInfo }); ok {
		info := provider.Info()
		return shared.RuntimeMapRenderer{
			Available: info.Available, ProtocolVersion: info.ProtocolVersion, Version: info.Version,
			Artifacts: append([]string(nil), info.Artifacts...), Error: info.Error,
		}
	}
	available, _ := m.renderer.Available()
	return shared.RuntimeMapRenderer{Available: available, Artifacts: []string{}}
}

func (m *Manager) PrepareSnapshot(ctx context.Context, transferID, cluster, shard, sessionID, fileName string) (Descriptor, error) {
	return m.prepare(ctx, transferID, "snapshot", cluster, shard, sessionID, fileName, nil)
}

func (m *Manager) Render(ctx context.Context, transferID, cluster, shard, sessionID, fileName string, layers []worldmap.Layer) (Descriptor, error) {
	return m.prepare(ctx, transferID, "artifacts", cluster, shard, sessionID, fileName, layers)
}

func (m *Manager) prepare(ctx context.Context, transferID, kind, cluster, shard, sessionID, fileName string, layers []worldmap.Layer) (Descriptor, error) {
	if !safeIdentity(transferID) || kind != "snapshot" && kind != "artifacts" {
		return Descriptor{}, ErrInvalidRequest
	}
	if existing, err := m.descriptor(transferID); err == nil {
		if existing.Kind != kind || existing.Cluster != cluster || existing.Shard != shard || existing.SessionID != sessionID || existing.FileName != fileName {
			return Descriptor{}, ErrInvalidRequest
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Descriptor{}, err
	}
	source, err := m.sessionPath(cluster, shard, sessionID, fileName)
	if err != nil {
		return Descriptor{}, err
	}
	temporary, err := os.MkdirTemp(m.stateRoot, ".map-stage-")
	if err != nil {
		return Descriptor{}, err
	}
	defer os.RemoveAll(temporary)
	snapshotPath := filepath.Join(temporary, "session.snapshot")
	sourceSHA256, err := copySnapshot(ctx, source, snapshotPath)
	if err != nil {
		return Descriptor{}, err
	}
	payloadPath := filepath.Join(temporary, "payload.bin")
	logText := ""
	if kind == "snapshot" {
		if err := os.Rename(snapshotPath, payloadPath); err != nil {
			return Descriptor{}, err
		}
	} else {
		if available, _ := m.renderer.Available(); !available {
			return Descriptor{}, worldmap.ErrRendererUnavailable
		}
		artifacts := filepath.Join(temporary, "artifacts")
		if err := os.Mkdir(artifacts, 0o700); err != nil {
			return Descriptor{}, err
		}
		logBuffer := &boundedLog{limit: maximumRendererLog}
		if err := m.renderer.Render(ctx, snapshotPath, artifacts, layers, logBuffer); err != nil {
			return Descriptor{}, fmt.Errorf("render map: %w", err)
		}
		logText = sanitizeLog(logBuffer.String(), m.saveRoot, m.stateRoot, source, temporary)
		if err := archiveArtifacts(artifacts, payloadPath); err != nil {
			return Descriptor{}, err
		}
		if err := os.Remove(snapshotPath); err != nil {
			return Descriptor{}, err
		}
		if err := os.RemoveAll(artifacts); err != nil {
			return Descriptor{}, err
		}
	}
	size, digest, err := describeFile(payloadPath)
	if err != nil {
		return Descriptor{}, err
	}
	if size <= 0 || size > MaximumTransfer {
		return Descriptor{}, errors.New("map transfer exceeds the supported size")
	}
	descriptor := Descriptor{
		TransferID: transferID, Kind: kind, Cluster: cluster, Shard: shard, SessionID: sessionID, FileName: fileName,
		Size: size, SHA256: digest, SourceSHA256: sourceSHA256, Log: logText,
	}
	if err := writeJSON(filepath.Join(temporary, "descriptor.json"), descriptor); err != nil {
		return Descriptor{}, err
	}
	if err := os.Rename(temporary, m.transferRoot(transferID)); err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

func (m *Manager) Read(ctx context.Context, transferID string, offset int64) (Chunk, error) {
	if !safeIdentity(transferID) || offset < 0 {
		return Chunk{}, ErrInvalidRequest
	}
	descriptor, err := m.descriptor(transferID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Chunk{}, ErrTransferMissing
		}
		return Chunk{}, err
	}
	if offset > descriptor.Size {
		return Chunk{}, ErrInvalidRequest
	}
	file, err := os.Open(filepath.Join(m.transferRoot(transferID), "payload.bin"))
	if err != nil {
		return Chunk{}, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return Chunk{}, err
	}
	remaining := descriptor.Size - offset
	readSize := int64(shared.MaxChunkBytes)
	if remaining < readSize {
		readSize = remaining
	}
	data := make([]byte, readSize)
	if readSize > 0 {
		if _, err := io.ReadFull(&contextReader{ctx: ctx, reader: file}, data); err != nil {
			return Chunk{}, err
		}
	}
	next := offset + int64(len(data))
	descriptor.Log = ""
	return Chunk{Descriptor: descriptor, Offset: offset, NextOffset: next, Data: data, Complete: next == descriptor.Size}, nil
}

func (m *Manager) Release(transferID string) error {
	if !safeIdentity(transferID) {
		return ErrInvalidRequest
	}
	if err := os.RemoveAll(m.transferRoot(transferID)); err != nil {
		return err
	}
	return nil
}

func (m *Manager) sessionRoot(cluster, shard string) (string, error) {
	clusterPath, err := safeDirectory(m.saveRoot, cluster)
	if err != nil {
		return "", err
	}
	shardPath, err := safeDirectory(clusterPath, shard)
	if err != nil {
		return "", err
	}
	savePath, err := safeDirectory(shardPath, "save")
	if err != nil {
		return "", err
	}
	return safeDirectory(savePath, "session")
}

func (m *Manager) sessionPath(cluster, shard, sessionID, fileName string) (string, error) {
	root, err := m.sessionRoot(cluster, shard)
	if err != nil {
		return "", err
	}
	directory, err := safeDirectory(root, sessionID)
	if err != nil || !safeComponent(fileName) {
		return "", ErrInvalidRequest
	}
	path := filepath.Join(directory, fileName)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maximumSnapshot {
		return "", ErrInvalidRequest
	}
	return path, nil
}

func (m *Manager) transferRoot(transferID string) string {
	return filepath.Join(m.stateRoot, transferID)
}

func (m *Manager) descriptor(transferID string) (Descriptor, error) {
	var value Descriptor
	data, err := os.ReadFile(filepath.Join(m.transferRoot(transferID), "descriptor.json"))
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.TransferID != transferID || !safeComponent(value.Cluster) || !safeComponent(value.Shard) ||
		!safeComponent(value.SessionID) || !safeComponent(value.FileName) || value.Kind != "snapshot" && value.Kind != "artifacts" ||
		value.Size <= 0 || value.Size > MaximumTransfer || len(value.SHA256) != 64 || len(value.SourceSHA256) != 64 {
		return Descriptor{}, ErrInvalidRequest
	}
	return value, nil
}

func (m *Manager) cleanupExpired(now time.Time) error {
	entries, err := os.ReadDir(m.stateRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safeIdentity(entry.Name()) && !strings.HasPrefix(entry.Name(), ".map-stage-") {
			continue
		}
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > stagingRetention {
			if err := os.RemoveAll(filepath.Join(m.stateRoot, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func archiveArtifacts(root, destination string) error {
	expected := []string{maprenderer.TerrainFileName, maprenderer.IconsFileName, maprenderer.ManifestFileName, maprenderer.FeaturesFileName}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != len(expected) {
		return errors.New("renderer did not produce the required map artifacts")
	}
	allowed := make(map[string]bool, len(expected))
	for _, name := range expected {
		allowed[name] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || !allowed[entry.Name()] {
			return errors.New("renderer produced an unexpected map artifact")
		}
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(output)
	for _, name := range expected {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			_ = writer.Close()
			_ = output.Close()
			return errors.New("renderer artifact is unsafe")
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			_ = output.Close()
			return err
		}
		input, err := os.Open(filepath.Join(root, name))
		if err != nil {
			_ = writer.Close()
			_ = output.Close()
			return err
		}
		_, copyErr := io.Copy(entry, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			_ = writer.Close()
			_ = output.Close()
			return errors.Join(copyErr, closeErr)
		}
	}
	if err := writer.Close(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func copySnapshot(ctx context.Context, sourcePath, targetPath string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maximumSnapshot {
		return "", ErrInvalidRequest
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hash), &contextReader{ctx: ctx, reader: io.LimitReader(source, maximumSnapshot+1)})
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(targetPath)
		return "", errors.Join(copyErr, closeErr)
	}
	after, err := source.Stat()
	if err != nil || written != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		_ = os.Remove(targetPath)
		return "", errors.New("Session changed while it was copied; retry the operation")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func describeFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, MaximumTransfer+1))
	if err != nil || size > MaximumTransfer {
		return 0, "", errors.Join(err, ErrInvalidRequest)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func trustedRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		return "", ErrInvalidRequest
	}
	root := filepath.Clean(value)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(err, ErrInvalidRequest)
	}
	return root, nil
}

func safeDirectory(parent, component string) (string, error) {
	if !safeComponent(component) {
		return "", ErrInvalidRequest
	}
	path := filepath.Join(parent, component)
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(err, ErrInvalidRequest)
	}
	return path, nil
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n") && len(value) <= 255
}

func safeIdentity(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func sanitizeLog(value string, paths ...string) string {
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			value = strings.ReplaceAll(value, filepath.Clean(path), "[REDACTED_PATH]")
		}
	}
	return strings.TrimSpace(strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || character >= 0x20 && character != 0x7f {
			return character
		}
		return -1
	}, value))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}

type boundedLog struct {
	buffer bytes.Buffer
	limit  int
	cut    bool
}

func (b *boundedLog) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.cut = true
		}
		_, _ = b.buffer.Write(value)
	} else {
		b.cut = true
	}
	return original, nil
}

func (b *boundedLog) String() string {
	if b.cut {
		return b.buffer.String() + "\n[DST Admin] renderer output truncated"
	}
	return b.buffer.String()
}
