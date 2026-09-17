package runtimefiles

import (
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
	"reflect"
	"strings"
	"time"

	"dont/internal/dsttime"
	"dont/shared"
)

const (
	MaximumArtifactBytes = int64(1024 * 1024)
	MaximumBundleBytes   = int64(4 * 1024 * 1024)
	MaximumLogBytes      = 512 * 1024
)

var artifactNames = map[shared.ArtifactKind][]string{
	shared.ArtifactRuntimeHealth:        {"health.json"},
	shared.ArtifactRuntimePlayers:       {"players-a.json", "players-b.json"},
	shared.ArtifactRuntimePlayerHistory: {"player-history.json"},
	shared.ArtifactRuntimeWorldState:    {"worldstate-a.json", "worldstate-b.json"},
	shared.ArtifactRuntimeEvents:        {"events-a.json", "events-b.json"},
	shared.ArtifactRuntimeCommand:       {"command-receipt-a.json", "command-receipt-b.json"},
	shared.ArtifactRuntimeDiagnostics:   {"diagnostic-a.json", "diagnostic-b.json"},
	shared.ArtifactRuntimeBarrier:       {"snapshot-barrier.json"},
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
	if kind == shared.ArtifactRuntimePlayerHistory {
		return readPlayerHistoryArtifact(ctx, root, shard)
	}
	root = filepath.Join(root, "save", "mod_config_data", "dst-admin")
	bundle := shared.RuntimeArtifactBundle{Kind: kind, Artifacts: []shared.RuntimeArtifact{}}
	var total int64
	var readFailures error
	for _, name := range artifactNames[kind] {
		if err := ctx.Err(); err != nil {
			return bundle, err
		}
		path := filepath.Join(root, name)
		data, info, exists, err := readTrustedRegular(path, MaximumArtifactBytes)
		if err != nil {
			if kind == shared.ArtifactRuntimeWorldState {
				readFailures = errors.Join(readFailures, fmt.Errorf("%s: %w", name, err))
				continue
			}
			return bundle, err
		}
		if !exists {
			continue
		}
		total += int64(len(data))
		if total > MaximumBundleBytes {
			return bundle, errors.New("Runtime 制品集合超过 4 MiB")
		}
		sum := sha256.Sum256(data)
		bundle.Artifacts = append(bundle.Artifacts, shared.RuntimeArtifact{
			Name: name, Size: info.Size(), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: info.ModTime().UTC(), Data: data,
		})
	}
	if len(bundle.Artifacts) == 0 {
		if readFailures != nil {
			return bundle, readFailures
		}
		return bundle, os.ErrNotExist
	}
	return bundle, nil
}

// ValidateArtifactBundle treats Driver output as untrusted even when it came
// from an authenticated Agent. This catches transport truncation and a
// compromised or stale Agent before Runtime JSON is decoded by callers.
func ValidateArtifactBundle(kind shared.ArtifactKind, bundle shared.RuntimeArtifactBundle) error {
	names, exists := artifactNames[kind]
	if !exists || bundle.Kind != kind || len(bundle.Artifacts) < 1 || len(bundle.Artifacts) > len(names) {
		return errors.New("Runtime 制品集合元数据无效")
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(bundle.Artifacts))
	var total int64
	maximumArtifactBytes := MaximumArtifactBytes
	if kind == shared.ArtifactRuntimePlayerHistory {
		maximumArtifactBytes = MaximumBundleBytes
	}
	for _, artifact := range bundle.Artifacts {
		if !allowed[artifact.Name] || seen[artifact.Name] || artifact.Size < 1 || artifact.Size > maximumArtifactBytes ||
			artifact.Size != int64(len(artifact.Data)) || artifact.UpdatedAt.IsZero() || len(artifact.SHA256) != sha256.Size*2 {
			return errors.New("Runtime 制品元数据无效")
		}
		sum := sha256.Sum256(artifact.Data)
		if !strings.EqualFold(artifact.SHA256, hex.EncodeToString(sum[:])) {
			return errors.New("Runtime 制品校验和不匹配")
		}
		total += artifact.Size
		if total > MaximumBundleBytes {
			return errors.New("Runtime 制品集合超过 4 MiB")
		}
		seen[artifact.Name] = true
	}
	return nil
}

// DecodeJSONArtifact accepts the optional header written by DST's
// SetPersistentString while preserving strict JSON field and trailing-data
// checks for Runtime artifacts.
func DecodeJSONArtifact(data []byte, destination interface{}) error {
	payload, err := persistentJSONPayload(data)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON content")
		}
		return err
	}
	return nil
}

func persistentJSONPayload(data []byte) ([]byte, error) {
	data = bytes.TrimSpace(data)
	if !bytes.HasPrefix(data, []byte("KLEI")) {
		return data, nil
	}
	index := len("KLEI")
	if index >= len(data) || !isJSONHeaderSpace(data[index]) {
		return nil, errors.New("invalid KLEI persistent JSON header")
	}
	for index < len(data) && isJSONHeaderSpace(data[index]) {
		index++
	}
	versionStart := index
	for index < len(data) && data[index] >= '0' && data[index] <= '9' {
		index++
	}
	if versionStart == index || index-versionStart > 10 || index >= len(data) || !isJSONHeaderSpace(data[index]) {
		return nil, errors.New("invalid KLEI persistent JSON header")
	}
	for index < len(data) && isJSONHeaderSpace(data[index]) {
		index++
	}
	payload := bytes.TrimSpace(data[index:])
	if len(payload) == 0 || payload[0] != '{' {
		return nil, errors.New("invalid KLEI persistent JSON payload")
	}
	return payload, nil
}

func isJSONHeaderSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

// ValidateLogChunk verifies cursor and payload invariants without trusting
// the remote filesystem metadata returned by an Agent.
func ValidateLogChunk(request shared.RuntimeLogRequest, chunk shared.RuntimeLogChunk) error {
	allowedNames, validSource := runtimeLogNames(request.Source)
	if filepath.Base(chunk.FileName) != chunk.FileName ||
		!validSource || !containsRuntimeLogName(allowedNames, chunk.FileName) ||
		chunk.FileID == "" || len(chunk.FileID) > 128 || strings.ContainsAny(chunk.FileID, "\x00\r\n") ||
		chunk.Size < 0 || chunk.Cursor < 0 || chunk.Cursor > chunk.Size || chunk.UpdatedAt.IsZero() {
		return errors.New("Runtime 日志块元数据无效")
	}
	if request.FileID != "" && request.FileID != chunk.FileID && !chunk.Reset {
		return errors.New("Runtime 日志轮转标识无效")
	}
	if request.Raw {
		if len(chunk.Lines) != 0 || chunk.Truncated || len(chunk.Data) > request.MaxBytes {
			return errors.New("Runtime 原始日志块负载无效")
		}
		start := request.Cursor
		if chunk.Reset {
			start = 0
		} else if start < 0 {
			start = chunk.Size - int64(request.MaxBytes)
			if start < 0 {
				start = 0
			}
		}
		if start < 0 || chunk.Cursor-start != int64(len(chunk.Data)) {
			return errors.New("Runtime 原始日志块游标与数据长度不一致")
		}
		return nil
	}
	if len(chunk.Data) != 0 || len(chunk.Lines) > request.MaxLines {
		return errors.New("Runtime 日志行负载无效")
	}
	previous := int64(-1)
	for _, line := range chunk.Lines {
		if line.Cursor < 0 || line.Cursor > chunk.Cursor || line.Cursor <= previous || strings.ContainsAny(line.Text, "\r\n") {
			return errors.New("Runtime 日志行游标无效")
		}
		previous = line.Cursor
	}
	return nil
}

func ReadLogs(ctx context.Context, saveRoot, cluster, shard string, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	logNames, validSource := runtimeLogNames(request.Source)
	if !validSource {
		return shared.RuntimeLogChunk{}, errors.New("Runtime 日志来源不受支持")
	}
	worldRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	var path string
	var info os.FileInfo
	for _, name := range logNames {
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
	startedAt := runtimeLogStartTime(worldRoot, request.Source, file)
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
	if request.Raw {
		return shared.RuntimeLogChunk{
			FileName: filepath.Base(path), FileID: fileID, Size: info.Size(), Cursor: start + int64(len(data)),
			Reset: reset, StartedAt: startedAt, UpdatedAt: info.ModTime().UTC(), Lines: []shared.RuntimeLogLine{}, Data: data,
		}, nil
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
		Truncated: truncated, StartedAt: startedAt, UpdatedAt: info.ModTime().UTC(), Lines: lines,
	}, nil
}

func runtimeLogNames(source shared.RuntimeLogSource) ([]string, bool) {
	switch source {
	case "", shared.RuntimeLogSourceServer:
		return []string{"server_log.txt", "forest_server_log.txt"}, true
	case shared.RuntimeLogSourceChat:
		return []string{"server_chat_log.txt"}, true
	default:
		return nil, false
	}
}

func containsRuntimeLogName(names []string, value string) bool {
	for _, name := range names {
		if name == value {
			return true
		}
	}
	return false
}

func readLogStartTime(file *os.File) time.Time {
	position, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return time.Time{}
	}
	defer func() { _, _ = file.Seek(position, io.SeekStart) }()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return time.Time{}
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024))
	if err != nil {
		return time.Time{}
	}
	startedAt, ok := dsttime.FindStartTime(string(data))
	if !ok {
		return time.Time{}
	}
	return startedAt
}

func runtimeLogStartTime(worldRoot string, source shared.RuntimeLogSource, selected *os.File) time.Time {
	if source != shared.RuntimeLogSourceChat {
		return readLogStartTime(selected)
	}
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		path := filepath.Join(worldRoot, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		startedAt := readLogStartTime(file)
		_ = file.Close()
		if !startedAt.IsZero() {
			return startedAt
		}
	}
	return time.Time{}
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
	generation, err := logGenerationMarker(file)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(name + "\x00" + identity + "\x00" + generation))
	return hex.EncodeToString(sum[:16]), nil
}

func logGenerationMarker(file *os.File) (string, error) {
	const maximumProbeBytes = 512
	probe := make([]byte, maximumProbeBytes)
	read, err := file.ReadAt(probe, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	probe = probe[:read]
	if newline := bytes.IndexByte(probe, '\n'); newline >= 0 {
		probe = probe[:newline+1]
	}
	sum := sha256.Sum256(probe)
	return hex.EncodeToString(sum[:8]), nil
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
