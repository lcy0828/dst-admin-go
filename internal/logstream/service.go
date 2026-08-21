package logstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/dsttime"
	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/shared"
)

const (
	maxReadBytes = int64(8 * 1024 * 1024)
	maxLineBytes = 1024 * 1024
)

var (
	ErrLogNotFound = errors.New("world log not found")
	ErrUnsafeLog   = errors.New("world log path is unsafe")
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
	Worlds(string) ([]rooms.World, error)
}

type Line struct {
	Cursor int64  `json:"cursor"`
	Text   string `json:"text"`
}

type Snapshot struct {
	FileName  string    `json:"fileName"`
	Size      int64     `json:"size"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Truncated bool      `json:"truncated"`
	Lines     []Line    `json:"lines"`
}

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type WorldSnapshot struct {
	WorldID   string          `json:"worldId"`
	WorldName string          `json:"worldName"`
	WorldRole rooms.WorldRole `json:"worldRole"`
	ReadAt    time.Time       `json:"readAt"`
	Snapshot  *Snapshot       `json:"snapshot,omitempty"`
	Problem   *Problem        `json:"problem,omitempty"`
}

type RoomSnapshot struct {
	RoomID      string          `json:"roomId"`
	ReadAt      time.Time       `json:"readAt"`
	Partial     bool            `json:"partial"`
	Available   int             `json:"available"`
	Unavailable int             `json:"unavailable"`
	Worlds      []WorldSnapshot `json:"worlds"`
}

type Event struct {
	Type     string    `json:"type"`
	Line     *Line     `json:"line,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

type DownloadInfo struct {
	FileName  string
	Size      int64
	UpdatedAt time.Time
}

type DistributedReader interface {
	ReadLogs(context.Context, string, string, shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error)
}

type Service struct {
	root         string
	rooms        RoomCatalog
	pollInterval time.Duration
	distributed  DistributedReader
}

func NewService(saveRoot string, roomCatalog RoomCatalog, readers ...DistributedReader) (*Service, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	if roomCatalog == nil {
		return nil, errors.New("room catalog is required")
	}
	service := &Service{root: filepath.Clean(absolute), rooms: roomCatalog, pollInterval: 500 * time.Millisecond}
	if len(readers) > 0 {
		service.distributed = readers[0]
	}
	return service, nil
}

func (s *Service) Snapshot(roomID, worldID string, limit int, query string) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.SnapshotContext(ctx, roomID, worldID, limit, query)
}

func (s *Service) SnapshotContext(ctx context.Context, roomID, worldID string, limit int, query string) (Snapshot, error) {
	return s.snapshotContext(ctx, roomID, worldID, shared.RuntimeLogSourceServer, limit, query)
}

func (s *Service) ChatSnapshot(roomID, worldID string, limit int, query string) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.ChatSnapshotContext(ctx, roomID, worldID, limit, query)
}

func (s *Service) ChatSnapshotContext(ctx context.Context, roomID, worldID string, limit int, query string) (Snapshot, error) {
	return s.snapshotContext(ctx, roomID, worldID, shared.RuntimeLogSourceChat, limit, query)
}

func (s *Service) snapshotContext(ctx context.Context, roomID, worldID string, source shared.RuntimeLogSource, limit int, query string) (Snapshot, error) {
	if s.distributed != nil {
		return s.distributedSnapshot(ctx, roomID, worldID, source, limit, query)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	path, info, err := s.locateSource(roomID, worldID, source)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotAt(path, info, limit, query)
}

func (s *Service) RoomSnapshot(ctx context.Context, roomID string, limit int, query string) (RoomSnapshot, error) {
	return s.roomSnapshot(ctx, roomID, shared.RuntimeLogSourceServer, limit, query)
}

func (s *Service) RoomChatSnapshot(ctx context.Context, roomID string, limit int, query string) (RoomSnapshot, error) {
	return s.roomSnapshot(ctx, roomID, shared.RuntimeLogSourceChat, limit, query)
}

func (s *Service) roomSnapshot(ctx context.Context, roomID string, source shared.RuntimeLogSource, limit int, query string) (RoomSnapshot, error) {
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return RoomSnapshot{}, err
	}
	result := RoomSnapshot{
		RoomID: roomID,
		ReadAt: time.Now().UTC(),
		Worlds: make([]WorldSnapshot, len(worlds)),
	}
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for index, world := range worlds {
		index, world := index, world
		wait.Add(1)
		go func() {
			defer wait.Done()
			item := WorldSnapshot{
				WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role,
			}
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				item.ReadAt = time.Now().UTC()
				item.Problem = logProblem(ctx.Err(), source)
				result.Worlds[index] = item
				return
			}
			worldContext, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			snapshot, snapshotErr := s.snapshotContext(worldContext, roomID, world.ID, source, limit, query)
			item.ReadAt = time.Now().UTC()
			if snapshotErr != nil {
				item.Problem = logProblem(snapshotErr, source)
			} else {
				item.Snapshot = &snapshot
			}
			result.Worlds[index] = item
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return RoomSnapshot{}, err
	}
	for _, world := range result.Worlds {
		if world.Problem == nil {
			result.Available++
		} else {
			result.Unavailable++
		}
	}
	result.Partial = result.Available > 0 && result.Unavailable > 0
	return result, nil
}

func snapshotAt(path string, info os.FileInfo, limit int, query string) (Snapshot, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	lines, truncated, err := readTail(path, info.Size(), limit, strings.TrimSpace(query))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{FileName: filepath.Base(path), Size: info.Size(), StartedAt: snapshotStartTime(path), UpdatedAt: info.ModTime().UTC(), Truncated: truncated, Lines: lines}, nil
}

func (s *Service) Open(roomID, worldID string) (*os.File, os.FileInfo, error) {
	path, info, err := s.locate(roomID, worldID)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open world log: %w", err)
	}
	return file, info, nil
}

func (s *Service) Download(ctx context.Context, roomID, worldID string, emit func(DownloadInfo, []byte) error) error {
	if emit == nil {
		return errors.New("log download emitter is required")
	}
	if s.distributed == nil {
		file, info, err := s.Open(roomID, worldID)
		if err != nil {
			return err
		}
		defer file.Close()
		buffer := make([]byte, runtimefiles.MaximumLogBytes)
		metadata := DownloadInfo{FileName: info.Name(), Size: info.Size(), UpdatedAt: info.ModTime().UTC()}
		for {
			count, readErr := file.Read(buffer)
			if count > 0 {
				if err := emit(metadata, buffer[:count]); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	var fileID string
	var cursor int64
	for {
		chunk, err := s.distributed.ReadLogs(ctx, roomID, worldID, shared.RuntimeLogRequest{
			FileID: fileID, Cursor: cursor, MaxBytes: runtimefiles.MaximumLogBytes, Raw: true,
		})
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrLogNotFound
			}
			return err
		}
		if fileID != "" && (chunk.Reset || chunk.FileID != fileID) {
			return errors.New("服务器日志在下载期间发生轮转，请重新下载")
		}
		if chunk.Cursor < cursor || chunk.Cursor > chunk.Size || chunk.Cursor == cursor && cursor < chunk.Size {
			return errors.New("远程日志下载游标没有继续前进")
		}
		fileID, cursor = chunk.FileID, chunk.Cursor
		if err := emit(DownloadInfo{FileName: chunk.FileName, Size: chunk.Size, UpdatedAt: chunk.UpdatedAt}, chunk.Data); err != nil {
			return err
		}
		if cursor >= chunk.Size {
			return nil
		}
	}
}

func (s *Service) Follow(ctx context.Context, roomID, worldID string, tail int, emit func(Event) error) error {
	if emit == nil {
		return errors.New("log event emitter is required")
	}
	if s.distributed != nil {
		return s.followDistributed(ctx, roomID, worldID, tail, emit)
	}
	path, info, err := s.locate(roomID, worldID)
	if err != nil {
		return err
	}
	if tail <= 0 || tail > 2000 {
		tail = 200

	}
	snapshot, err := snapshotAt(path, info, tail, "")
	if err != nil {
		return err
	}
	if err := emit(Event{Type: "connected", Snapshot: &snapshot}); err != nil {
		return err
	}
	position := info.Size()
	identity := info
	pending := ""
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			current, statErr := os.Lstat(path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					return ErrLogNotFound
				}
				return fmt.Errorf("inspect world log: %w", statErr)
			}
			if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
				return ErrUnsafeLog
			}
			if !os.SameFile(identity, current) || current.Size() < position {
				identity = current
				position = 0
				pending = ""
				reset := Snapshot{
					FileName:  filepath.Base(path),
					Size:      current.Size(),
					StartedAt: snapshotStartTime(path),
					UpdatedAt: current.ModTime().UTC(),
				}
				if err := emit(Event{Type: "reset", Snapshot: &reset}); err != nil {
					return err
				}
			}
			if current.Size() == position {
				continue
			}
			lines, next, remainder, readErr := readAppended(path, position, pending)
			if readErr != nil {
				return readErr
			}
			position, pending = next, remainder
			for index := range lines {
				if err := emit(Event{Type: "line", Line: &lines[index]}); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Service) distributedSnapshot(ctx context.Context, roomID, worldID string, source shared.RuntimeLogSource, limit int, query string) (Snapshot, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	chunk, err := s.distributed.ReadLogs(ctx, roomID, worldID, shared.RuntimeLogRequest{
		Source: source, Cursor: -1, MaxBytes: runtimefiles.MaximumLogBytes, MaxLines: limit, Query: strings.TrimSpace(query),
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, ErrLogNotFound
		}
		return Snapshot{}, err
	}
	return snapshotFromChunk(chunk), nil
}

func logProblem(err error, source shared.RuntimeLogSource) *Problem {
	resourceName := "服务器日志"
	if source == shared.RuntimeLogSourceChat {
		resourceName = "聊天日志"
	}
	switch {
	case errors.Is(err, ErrLogNotFound), errors.Is(err, os.ErrNotExist):
		return &Problem{Code: "LOG_NOT_FOUND", Message: "该分片还没有生成" + resourceName}
	case errors.Is(err, context.DeadlineExceeded):
		return &Problem{Code: "LOG_READ_TIMEOUT", Message: "读取分片日志超时"}
	case errors.Is(err, context.Canceled):
		return &Problem{Code: "LOG_READ_CANCELED", Message: "读取分片日志已取消"}
	case errors.Is(err, ErrUnsafeLog), errors.Is(err, rooms.ErrUnsafePath), errors.Is(err, rooms.ErrInvalidID):
		return &Problem{Code: "UNSAFE_LOG_PATH", Message: "日志资源路径无效"}
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		return &Problem{Code: "RESOURCE_NOT_FOUND", Message: "房间或世界不存在"}
	default:
		return &Problem{Code: "LOG_READ_FAILED", Message: "读取" + resourceName + "失败"}
	}
}

func (s *Service) followDistributed(ctx context.Context, roomID, worldID string, tail int, emit func(Event) error) error {
	if tail <= 0 || tail > 2000 {
		tail = 200
	}
	chunk, err := s.distributed.ReadLogs(ctx, roomID, worldID, shared.RuntimeLogRequest{
		Cursor: -1, MaxBytes: runtimefiles.MaximumLogBytes, MaxLines: tail,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrLogNotFound
		}
		return err
	}
	if err := emit(Event{Type: "connected", Snapshot: snapshotPointer(snapshotFromChunk(chunk))}); err != nil {
		return err
	}
	fileID, cursor := chunk.FileID, chunk.Cursor
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			next, readErr := s.distributed.ReadLogs(ctx, roomID, worldID, shared.RuntimeLogRequest{
				FileID: fileID, Cursor: cursor, MaxBytes: runtimefiles.MaximumLogBytes, MaxLines: 2000,
			})
			if readErr != nil {
				return readErr
			}
			if next.Reset || next.FileID != fileID {
				reset := Snapshot{
					FileName:  next.FileName,
					Size:      next.Size,
					StartedAt: next.StartedAt,
					UpdatedAt: next.UpdatedAt,
				}
				if err := emit(Event{Type: "reset", Snapshot: &reset}); err != nil {
					return err
				}
			}
			fileID, cursor = next.FileID, next.Cursor
			for _, line := range next.Lines {
				current := Line{Cursor: line.Cursor, Text: line.Text}
				if err := emit(Event{Type: "line", Line: &current}); err != nil {
					return err
				}
			}
		}
	}
}

func snapshotFromChunk(chunk shared.RuntimeLogChunk) Snapshot {
	lines := make([]Line, 0, len(chunk.Lines))
	for _, line := range chunk.Lines {
		lines = append(lines, Line{Cursor: line.Cursor, Text: line.Text})
	}
	return Snapshot{
		FileName: chunk.FileName, Size: chunk.Size, StartedAt: chunk.StartedAt, UpdatedAt: chunk.UpdatedAt,
		Truncated: chunk.Truncated, Lines: lines,
	}
}

func readLogStartTime(path string) time.Time {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer file.Close()
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

func snapshotStartTime(path string) time.Time {
	if filepath.Base(path) != "server_chat_log.txt" {
		return readLogStartTime(path)
	}
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
		candidate := filepath.Join(filepath.Dir(path), name)
		info, err := os.Lstat(candidate)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if startedAt := readLogStartTime(candidate); !startedAt.IsZero() {
			return startedAt
		}
	}
	return time.Time{}
}

func snapshotPointer(value Snapshot) *Snapshot { return &value }

func (s *Service) locate(roomID, worldID string) (string, os.FileInfo, error) {
	return s.locateSource(roomID, worldID, shared.RuntimeLogSourceServer)
}

func (s *Service) locateSource(roomID, worldID string, source shared.RuntimeLogSource) (string, os.FileInfo, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return "", nil, err
	}
	world, err := s.rooms.World(roomID, worldID)
	if err != nil {
		return "", nil, err
	}
	worldPath := filepath.Join(s.root, room.DirectoryName, world.DirectoryName)
	if !contained(s.root, worldPath) {
		return "", nil, ErrUnsafeLog
	}
	type candidate struct {
		path string
		info os.FileInfo
	}
	var candidates []candidate
	names := []string{"server_log.txt", "forest_server_log.txt"}
	if source == shared.RuntimeLogSourceChat {
		names = []string{"server_chat_log.txt"}
	}
	for _, name := range names {
		path := filepath.Join(worldPath, name)
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", nil, fmt.Errorf("inspect world log: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", nil, ErrUnsafeLog
		}
		candidates = append(candidates, candidate{path: path, info: info})
	}
	if len(candidates) == 0 {
		return "", nil, ErrLogNotFound
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].info.ModTime().After(candidates[j].info.ModTime()) })
	return candidates[0].path, candidates[0].info, nil
}

func readTail(path string, size int64, limit int, query string) ([]Line, bool, error) {
	start := size - maxReadBytes
	truncated := start > 0
	if start < 0 {
		start = 0
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open world log: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("seek world log: %w", err)
	}
	readBytes := size - start
	if readBytes < 0 {
		readBytes = 0
	}
	if readBytes > maxReadBytes {
		readBytes = maxReadBytes
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, readBytes), 64*1024)
	cursor := start
	if start > 0 {
		skipped, _ := reader.ReadString('\n')
		cursor += int64(len(skipped))
	}
	needle := strings.ToLower(query)
	lines := make([]Line, 0, limit)
	for {
		text, readErr := reader.ReadString('\n')
		cursor += int64(len(text))
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
		if text != "" && (needle == "" || strings.Contains(strings.ToLower(text), needle)) {
			lines = append(lines, Line{Cursor: cursor, Text: text})
			if len(lines) > limit {
				lines = lines[len(lines)-limit:]
				truncated = true
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, false, fmt.Errorf("read world log: %w", readErr)
		}
	}
	return lines, truncated, nil
}

func readAppended(path string, position int64, pending string) ([]Line, int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, position, pending, fmt.Errorf("open world log: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(position, io.SeekStart); err != nil {
		return nil, position, pending, fmt.Errorf("seek world log: %w", err)
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	lines := make([]Line, 0)
	cursor := position
	for {
		part, readErr := reader.ReadString('\n')
		cursor += int64(len(part))
		if len(pending)+len(part) > maxLineBytes {
			return nil, position, pending, fmt.Errorf("world log line exceeds %d bytes", maxLineBytes)
		}
		part = pending + part
		pending = ""
		if strings.HasSuffix(part, "\n") {
			text := strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r")
			if text != "" {
				lines = append(lines, Line{Cursor: cursor, Text: text})
			}
		} else {
			pending = part
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, position, pending, fmt.Errorf("read world log: %w", readErr)
		}
	}
	return lines, cursor, pending, nil
}

func contained(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}
