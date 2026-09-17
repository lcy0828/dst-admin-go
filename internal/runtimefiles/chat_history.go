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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/shared"
)

const (
	maximumChatLogGenerations = 10000
	archivePairingWindow      = 10 * time.Second
)

var archivedChatLogPattern = regexp.MustCompile(`^server_chat_log_(\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})\.txt$`)
var archivedServerLogPattern = regexp.MustCompile(`^(?:server_log|forest_server_log)_(\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})\.txt$`)

var archiveStartTimes sync.Map

type chatLogCandidate struct {
	generation shared.RuntimeChatLogGeneration
	path       string
}

type serverLogCandidate struct {
	rotatedAt time.Time
	path      string
}

func ListChatLogGenerations(ctx context.Context, saveRoot, cluster, shard string) ([]shared.RuntimeChatLogGeneration, error) {
	candidates, err := chatLogCandidates(ctx, saveRoot, cluster, shard)
	if err != nil {
		return nil, err
	}
	result := make([]shared.RuntimeChatLogGeneration, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.generation)
	}
	return result, nil
}

func ValidateChatLogGenerations(generations []shared.RuntimeChatLogGeneration) error {
	if len(generations) > maximumChatLogGenerations {
		return errors.New("聊天日志代次数量超过安全上限")
	}
	seen := make(map[string]bool, len(generations))
	for _, generation := range generations {
		if !validChatGeneration(generation) || seen[generation.ID] {
			return errors.New("聊天日志代次元数据无效")
		}
		seen[generation.ID] = true
	}
	return nil
}

func ValidateChatLogResult(request shared.RuntimeChatLogRequest, result shared.RuntimeChatLogResult) error {
	if result.Generation == nil || result.Generation.ID != request.GenerationID || !validChatGeneration(*result.Generation) ||
		result.Cursor < request.Cursor || result.Cursor > result.Generation.Size || result.Complete != (result.Cursor == result.Generation.Size) ||
		result.Cursor-request.Cursor > int64(request.MaxBytes) || len(result.Lines) > request.MaxLines || len(result.Generations) != 0 {
		return errors.New("聊天日志块元数据无效")
	}
	previous := request.Cursor
	for _, line := range result.Lines {
		if line.Cursor <= previous || line.Cursor > result.Cursor || int64(len(line.Text)) > line.Cursor-previous || strings.ContainsAny(line.Text, "\r\n") {
			return errors.New("聊天日志块游标无效")
		}
		previous = line.Cursor
	}
	if result.TimeVersion == 0 && (len(result.Times) > 0 || result.TimesReady) {
		return errors.New("聊天日志时间版本缺失")
	}
	if result.TimeVersion != 0 {
		if result.TimeVersion != shared.ChatTimeVersion || !request.ResolveTimes || result.ReadBytes < 0 || result.ReadBytes > int64(request.MaxBytes)*2 ||
			result.TimesReady && len(result.Times) != len(result.Lines) || !result.TimesReady && (len(result.Times) != 0 || len(result.Lines) != 0 || result.Cursor != request.Cursor) {
			return errors.New("聊天日志时间元数据无效")
		}
		for i, stamp := range result.Times {
			if i >= len(result.Lines) || stamp.Cursor != result.Lines[i].Cursor || stamp.OccurredAt != nil && (stamp.OccurredAt.Before(result.Generation.StartedAt) || stamp.OccurredAt.After(result.Generation.UpdatedAt)) {
				return errors.New("聊天日志事件时间无效")
			}
		}
	}
	return nil
}

func validChatGeneration(generation shared.RuntimeChatLogGeneration) bool {
	return generation.ID != "" && len(generation.ID) <= 128 && !strings.ContainsAny(generation.ID, "\x00\r\n") &&
		filepath.Base(generation.FileName) == generation.FileName &&
		(generation.FileName == "server_chat_log.txt" || archivedChatLogPattern.MatchString(generation.FileName)) &&
		generation.Size >= 0 && !generation.UpdatedAt.IsZero()
}

func ReadChatLogGeneration(ctx context.Context, saveRoot, cluster, shard string, request shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error) {
	if request.GenerationID == "" || len(request.GenerationID) > 128 || strings.ContainsAny(request.GenerationID, "\x00\r\n") ||
		request.Cursor < 0 || request.MaxBytes < 1 || request.MaxBytes > MaximumLogBytes || request.MaxLines < 1 || request.MaxLines > 2000 {
		return shared.RuntimeChatLogResult{}, errors.New("聊天日志代次读取请求无效")
	}
	candidates, err := chatLogCandidates(ctx, saveRoot, cluster, shard)
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	var selected *chatLogCandidate
	for index := range candidates {
		if candidates[index].generation.ID == request.GenerationID {
			selected = &candidates[index]
			break
		}
	}
	if selected == nil {
		return shared.RuntimeChatLogResult{}, os.ErrNotExist
	}
	var times map[int64]*time.Time
	var clockBytes int64
	if request.ResolveTimes {
		var ready bool
		times, ready, clockBytes, err = resolveChatLogTimes(ctx, selected, int64(request.MaxBytes))
		if err != nil {
			return shared.RuntimeChatLogResult{}, err
		}
		if !ready {
			generation := selected.generation
			return shared.RuntimeChatLogResult{Generation: &generation, Cursor: request.Cursor,
				Complete: request.Cursor == generation.Size, TimeVersion: shared.ChatTimeVersion, ReadBytes: clockBytes}, nil
		}
	}
	file, err := os.Open(selected.path)
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	if !info.Mode().IsRegular() || request.Cursor > info.Size() {
		return shared.RuntimeChatLogResult{}, errors.New("聊天日志代次已发生不可恢复的变化")
	}
	if _, err := file.Seek(request.Cursor, io.SeekStart); err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(request.MaxBytes)))
	if err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return shared.RuntimeChatLogResult{}, err
	}
	readBytes := int64(len(data))
	completeBytes := len(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if selected.generation.Archived && request.Cursor+int64(len(data)) == info.Size() {
			completeBytes = len(data)
		} else if index := bytes.LastIndexByte(data, '\n'); index >= 0 {
			completeBytes = index + 1
		} else {
			completeBytes = 0
		}
	}
	if completeBytes == 0 && len(data) == request.MaxBytes && request.Cursor+int64(len(data)) < info.Size() {
		return shared.RuntimeChatLogResult{}, errors.New("聊天日志单行超过允许的读取大小")
	}
	data = data[:completeBytes]
	cursor := request.Cursor
	lines := make([]shared.RuntimeLogLine, 0, min(bytes.Count(data, []byte{'\n'})+1, request.MaxLines))
	for len(data) > 0 {
		lineBytes := len(data)
		consumed := lineBytes
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			lineBytes, consumed = newline, newline+1
		}
		raw := data[:lineBytes]
		text := strings.TrimSuffix(string(raw), "\r")
		if text != "" && len(lines) >= request.MaxLines {
			break
		}
		cursor += int64(consumed)
		data = data[consumed:]
		if text != "" {
			lines = append(lines, shared.RuntimeLogLine{Cursor: cursor, Text: text})
		}
	}
	generation := selected.generation
	generation.Size = info.Size()
	generation.UpdatedAt = info.ModTime().UTC()
	result := shared.RuntimeChatLogResult{
		Generation: &generation, Cursor: cursor, Lines: lines, Complete: cursor >= info.Size(),
		ReadBytes: clockBytes + readBytes,
	}
	if request.ResolveTimes {
		result.TimeVersion, result.TimesReady = shared.ChatTimeVersion, true
		for _, line := range lines {
			result.Times = append(result.Times, shared.RuntimeChatLogTime{Cursor: line.Cursor, OccurredAt: times[line.Cursor]})
		}
	}
	return result, nil
}

func chatLogCandidates(ctx context.Context, saveRoot, cluster, shard string) ([]chatLogCandidate, error) {
	worldRoot, err := trustedShardPath(saveRoot, cluster, shard)
	if err != nil {
		return nil, err
	}
	candidates := make([]chatLogCandidate, 0)
	currentPath := filepath.Join(worldRoot, "server_chat_log.txt")
	if info, exists, err := trustedChatLogInfo(currentPath); err != nil {
		return nil, err
	} else if exists {
		startedAt := currentChatStartTime(worldRoot)
		generation, err := chatGeneration(cluster, shard, currentPath, info, false, startedAt, false)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, chatLogCandidate{generation: generation, path: currentPath})
	}
	archiveRoot := filepath.Join(worldRoot, "backup", "server_chat_log")
	entries, err := trustedDirectoryEntries(archiveRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var serverLogs []serverLogCandidate
	if len(entries) != 0 {
		serverLogs, err = archivedServerLogCandidates(worldRoot)
		if err != nil {
			return nil, err
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches := archivedChatLogPattern.FindStringSubmatch(entry.Name())
		if len(matches) != 2 {
			continue
		}
		path := filepath.Join(archiveRoot, entry.Name())
		info, exists, err := trustedChatLogInfo(path)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		startedAt, estimated := archivedChatStartTime(matches[1], info, serverLogs)
		generation, err := chatGeneration(cluster, shard, path, info, true, startedAt, estimated)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, chatLogCandidate{generation: generation, path: path})
		if len(candidates) > maximumChatLogGenerations {
			return nil, errors.New("聊天日志归档数量超过安全上限")
		}
	}
	deduplicated := make(map[string]chatLogCandidate, len(candidates))
	for _, candidate := range candidates {
		current, exists := deduplicated[candidate.generation.ID]
		if !exists || candidate.generation.Archived && !current.generation.Archived || candidate.generation.UpdatedAt.After(current.generation.UpdatedAt) {
			deduplicated[candidate.generation.ID] = candidate
		}
	}
	candidates = candidates[:0]
	for _, candidate := range deduplicated {
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].generation, candidates[j].generation
		if !left.StartedAt.Equal(right.StartedAt) {
			if left.StartedAt.IsZero() {
				return true
			}
			if right.StartedAt.IsZero() {
				return false
			}
			return left.StartedAt.Before(right.StartedAt)
		}
		return left.UpdatedAt.Before(right.UpdatedAt)
	})
	return candidates, nil
}

func chatGeneration(cluster, shard, path string, info os.FileInfo, archived bool, startedAt time.Time, estimated bool) (shared.RuntimeChatLogGeneration, error) {
	identity := ""
	if !startedAt.IsZero() && !estimated {
		identity = "started:" + startedAt.UTC().Format(time.RFC3339Nano)
	} else {
		file, err := os.Open(path)
		if err != nil {
			return shared.RuntimeChatLogGeneration{}, err
		}
		identity, err = runtimeFileID(file, "server_chat_log")
		_ = file.Close()
		if err != nil {
			return shared.RuntimeChatLogGeneration{}, err
		}
	}
	sum := sha256.Sum256([]byte("chat-v1\x00" + cluster + "\x00" + shard + "\x00" + identity))
	return shared.RuntimeChatLogGeneration{
		ID: hex.EncodeToString(sum[:16]), FileName: filepath.Base(path), Archived: archived, Size: info.Size(),
		StartedAt: startedAt.UTC(), StartedAtEstimated: estimated, UpdatedAt: info.ModTime().UTC(),
		TimeVersion: shared.ChatTimeVersion,
	}, nil
}

func currentChatStartTime(worldRoot string) time.Time {
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		if value := trustedLogStartTime(filepath.Join(worldRoot, name)); !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func archivedChatStartTime(suffix string, chatInfo os.FileInfo, serverLogs []serverLogCandidate) (time.Time, bool) {
	rotatedAt, parseErr := time.ParseInLocation("2006-01-02-15-04-05", suffix, time.Local)
	if parseErr == nil {
		index := sort.Search(len(serverLogs), func(index int) bool {
			return !serverLogs[index].rotatedAt.Before(rotatedAt)
		})
		left, right := index-1, index
		for left >= 0 || right < len(serverLogs) {
			candidateIndex := -1
			if left < 0 {
				candidateIndex, right = right, right+1
			} else if right >= len(serverLogs) {
				candidateIndex, left = left, left-1
			} else if rotatedAt.Sub(serverLogs[left].rotatedAt) <= serverLogs[right].rotatedAt.Sub(rotatedAt) {
				candidateIndex, left = left, left-1
			} else {
				candidateIndex, right = right, right+1
			}
			candidate := serverLogs[candidateIndex]
			delta := candidate.rotatedAt.Sub(rotatedAt)
			if delta < 0 {
				delta = -delta
			}
			if delta > archivePairingWindow {
				break
			}
			if value := cachedLogStartTime(candidate.path); !value.IsZero() {
				return value, false
			}
		}
		return rotatedAt, true
	}
	return chatInfo.ModTime(), true
}

func archivedServerLogCandidates(worldRoot string) ([]serverLogCandidate, error) {
	result := make([]serverLogCandidate, 0)
	for _, directory := range []string{"server_log", "forest_server_log"} {
		root := filepath.Join(worldRoot, "backup", directory)
		entries, err := trustedDirectoryEntries(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			matches := archivedServerLogPattern.FindStringSubmatch(entry.Name())
			if len(matches) != 2 {
				continue
			}
			rotatedAt, err := time.ParseInLocation("2006-01-02-15-04-05", matches[1], time.Local)
			if err != nil {
				continue
			}
			result = append(result, serverLogCandidate{rotatedAt: rotatedAt, path: filepath.Join(root, entry.Name())})
			if len(result) > maximumChatLogGenerations {
				return nil, errors.New("服务日志归档数量超过安全上限")
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].rotatedAt.Before(result[j].rotatedAt) })
	return result, nil
}

func cachedLogStartTime(path string) time.Time {
	if value, exists := archiveStartTimes.Load(path); exists {
		return value.(time.Time)
	}
	value := trustedLogStartTime(path)
	if !value.IsZero() {
		archiveStartTimes.Store(path, value)
	}
	return value
}

func trustedLogStartTime(path string) time.Time {
	info, exists, err := trustedChatLogInfo(path)
	if err != nil || !exists || info.Size() == 0 {
		return time.Time{}
	}
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer file.Close()
	return readLogStartTime(file)
}

func trustedChatLogInfo(path string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("聊天日志资源不是受信普通文件: %s", filepath.Base(path))
	}
	return info, true, nil
}

func trustedDirectoryEntries(path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("聊天日志归档目录不安全")
	}
	return os.ReadDir(path)
}
