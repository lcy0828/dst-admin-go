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
	"time"

	"dont/internal/rooms"
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
}

type Line struct {
	Cursor int64  `json:"cursor"`
	Text   string `json:"text"`
}

type Snapshot struct {
	FileName  string    `json:"fileName"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updatedAt"`
	Truncated bool      `json:"truncated"`
	Lines     []Line    `json:"lines"`
}

type Event struct {
	Type     string    `json:"type"`
	Line     *Line     `json:"line,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

type Service struct {
	root         string
	rooms        RoomCatalog
	pollInterval time.Duration
}

func NewService(saveRoot string, roomCatalog RoomCatalog) (*Service, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	if roomCatalog == nil {
		return nil, errors.New("room catalog is required")
	}
	return &Service{root: filepath.Clean(absolute), rooms: roomCatalog, pollInterval: 500 * time.Millisecond}, nil
}

func (s *Service) Snapshot(roomID, worldID string, limit int, query string) (Snapshot, error) {
	path, info, err := s.locate(roomID, worldID)
	if err != nil {
		return Snapshot{}, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	lines, truncated, err := readTail(path, info.Size(), limit, strings.TrimSpace(query))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{FileName: filepath.Base(path), Size: info.Size(), UpdatedAt: info.ModTime().UTC(), Truncated: truncated, Lines: lines}, nil
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

func (s *Service) Follow(ctx context.Context, roomID, worldID string, tail int, emit func(Event) error) error {
	if emit == nil {
		return errors.New("log event emitter is required")
	}
	path, info, err := s.locate(roomID, worldID)
	if err != nil {
		return err
	}
	if tail <= 0 || tail > 2000 {
		tail = 200

	}
	snapshot, err := s.Snapshot(roomID, worldID, tail, "")
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
				reset := Snapshot{FileName: filepath.Base(path), Size: current.Size(), UpdatedAt: current.ModTime().UTC()}
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

func (s *Service) locate(roomID, worldID string) (string, os.FileInfo, error) {
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
	for _, name := range []string{"server_log.txt", "forest_server_log.txt"} {
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
	reader := bufio.NewReaderSize(io.LimitReader(file, maxReadBytes), 64*1024)
	if start > 0 {
		_, _ = reader.ReadString('\n')
	}
	needle := strings.ToLower(query)
	lines := make([]Line, 0, limit)
	cursor := start
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
