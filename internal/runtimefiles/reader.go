package runtimefiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"dont/shared"
)

const (
	maximumArtifactBytes = int64(1024 * 1024)
	maximumBundleBytes   = int64(4 * 1024 * 1024)
	MaximumLogBytes      = 512 * 1024
)

var artifactNames = map[shared.ArtifactKind][]string{
	shared.ArtifactRuntimeHealth:      {"health.json"},
	shared.ArtifactRuntimePlayers:     {"players-a.json", "players-b.json"},
	shared.ArtifactRuntimeWorldState:  {"worldstate-a.json", "worldstate-b.json"},
	shared.ArtifactRuntimeEvents:      {"events-a.json", "events-b.json"},
	shared.ArtifactRuntimeCommand:     {"command-receipt-a.json", "command-receipt-b.json"},
	shared.ArtifactRuntimeDiagnostics: {"diagnostic-a.json", "diagnostic-b.json"},
}

func IsArtifactKind(kind shared.ArtifactKind) bool {
	_, exists := artifactNames[kind]
	return exists
}

func ReadArtifacts(ctx context.Context, saveRoot, cluster, shard string, kind shared.ArtifactKind) (shared.RuntimeArtifactBundle, error) {
	if !IsArtifactKind(kind) {
		return shared.RuntimeArtifactBundle{}, errors.New("Runtime 制品类型不受支持")
	}
	root, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	root = filepath.Join(root, "save", "mod_config_data", "dst-admin")
	bundle := shared.RuntimeArtifactBundle{Kind: kind, Artifacts: []shared.RuntimeArtifact{}}
	var total int64
	for _, name := range artifactNames[kind] {
		if err := ctx.Err(); err != nil {
			return bundle, err
		}
		path := filepath.Join(root, name)
		data, info, exists, err := readTrustedRegular(path, maximumArtifactBytes)
		if err != nil {
			return bundle, err
		}
		if !exists {
			continue
		}
		total += int64(len(data))
		if total > maximumBundleBytes {
			return bundle, errors.New("Runtime 制品集合超过 4 MiB")
		}
		sum := sha256.Sum256(data)
		bundle.Artifacts = append(bundle.Artifacts, shared.RuntimeArtifact{
			Name: name, Size: info.Size(), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: info.ModTime().UTC(), Data: data,
		})
	}
	if len(bundle.Artifacts) == 0 {
		return bundle, os.ErrNotExist
	}
	return bundle, nil
}

func ReadLogs(ctx context.Context, saveRoot, cluster, shard string, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	worldRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	var path string
	var info os.FileInfo
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		candidate := filepath.Join(worldRoot, name)
		current, statErr := os.Lstat(candidate)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return shared.RuntimeLogChunk{}, statErr
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return shared.RuntimeLogChunk{}, errors.New("Runtime 日志文件不安全")
		}
		if info == nil || current.ModTime().After(info.ModTime()) {
			path, info = candidate, current
		}
	}
	if info == nil {
		return shared.RuntimeLogChunk{}, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	defer file.Close()
	fileID, err := runtimeFileID(file, filepath.Base(path))
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	start := request.Cursor
	reset, truncated, skipPartial := false, false, false
	if request.FileID != "" && request.FileID != fileID {
		start, reset = 0, true
	} else if start < 0 {
		start = info.Size() - int64(request.MaxBytes)
		if start < 0 {
			start = 0
		} else {
			truncated = true
			skipPartial = start > 0
		}
	} else if start > info.Size() {
		start, reset = 0, true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(request.MaxBytes)))
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	if err := ctx.Err(); err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	if skipPartial {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			start += int64(index + 1)
			data = data[index+1:]
		} else {
			start += int64(len(data))
			data = nil
		}
	}
	completeBytes := len(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if index := bytes.LastIndexByte(data, '\n'); index >= 0 {
			completeBytes = index + 1
		} else {
			completeBytes = 0
		}
	}
	data = data[:completeBytes]
	cursor := start
	needle := strings.ToLower(strings.TrimSpace(request.Query))
	parts := bytes.Split(data, []byte{'\n'})
	if len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}
	lines := make([]shared.RuntimeLogLine, 0)
	for _, raw := range parts {
		cursor += int64(len(raw) + 1)
		text := strings.TrimSuffix(string(raw), "\r")
		if text == "" || needle != "" && !strings.Contains(strings.ToLower(text), needle) {
			continue
		}
		lines = append(lines, shared.RuntimeLogLine{Cursor: cursor, Text: text})
		if len(lines) > request.MaxLines {
			lines = lines[len(lines)-request.MaxLines:]
			truncated = true
		}
	}
	if completeBytes == 0 {
		cursor = start
	}
	return shared.RuntimeLogChunk{
		FileName: filepath.Base(path), FileID: fileID, Size: info.Size(), Cursor: cursor, Reset: reset,
		Truncated: truncated, UpdatedAt: info.ModTime().UTC(), Lines: lines,
	}, nil
}

func trustedShardPath(saveRoot, cluster, shard string) (string, error) {
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return "", errors.New("DST 存档根目录无效")
	}
	if filepath.Base(cluster) != cluster || filepath.Base(shard) != shard || cluster == "" || shard == "" {
		return "", errors.New("房间或分片目录无效")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("DST 存档根目录不可用")
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, cluster, shard))
	if err != nil {
		return "", errors.New("DST 分片目录不可用")
	}
	relative, err := filepath.Rel(resolvedRoot, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", errors.New("分片目录越出 DST 存档根目录")
	}
	return candidate, nil
}

func runtimeFileID(file *os.File, name string) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	identity := stableFileIdentity(info)
	if identity == "" {
		identity = fmt.Sprintf("fallback:%d:%d", info.ModTime().UnixNano(), info.Size())
	}
	sum := sha256.Sum256([]byte(name + "\x00" + identity))
	return hex.EncodeToString(sum[:16]), nil
}

func stableFileIdentity(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return ""
	}
	device, deviceOK := numericField(value, "Dev")
	inode, inodeOK := numericField(value, "Ino")
	if deviceOK && inodeOK {
		return fmt.Sprintf("unix:%d:%d", device, inode)
	}
	volume, volumeOK := numericField(value, "VolumeSerialNumber")
	high, highOK := numericField(value, "FileIndexHigh")
	low, lowOK := numericField(value, "FileIndexLow")
	if volumeOK && highOK && lowOK {
		return fmt.Sprintf("windows:%d:%d:%d", volume, high, low)
	}
	creation := value.FieldByName("CreationTime")
	for creation.IsValid() && creation.Kind() == reflect.Pointer {
		if creation.IsNil() {
			break
		}
		creation = creation.Elem()
	}
	if creation.IsValid() && creation.Kind() == reflect.Struct {
		high, highOK = numericField(creation, "HighDateTime")
		low, lowOK = numericField(creation, "LowDateTime")
		if highOK && lowOK {
			return fmt.Sprintf("windows-created:%d:%d", high, low)
		}
	}
	return ""
}

func numericField(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(field.Int()), true
	default:
		return 0, false
	}
}

func readTrustedRegular(path string, limit int64) ([]byte, os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, nil, false, errors.New("Runtime 制品文件不安全或超过大小限制")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false, err
	}
	return data, info, true, nil
}
