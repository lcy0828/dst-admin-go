package chatlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/logstream"
	"dont/internal/rooms"
	"dont/shared"
)

const (
	chatSyncChunkBytes = 64 * 1024
	chatSyncChunkLines = 100
	maximumSyncChunks  = 64
)

type WorldCatalog interface {
	Worlds(string) ([]rooms.World, error)
}

type GenerationReader interface {
	ListChatLogGenerations(context.Context, string, string) ([]shared.RuntimeChatLogGeneration, error)
	ReadChatLogGeneration(context.Context, string, string, shared.RuntimeChatLogRequest) (shared.RuntimeChatLogResult, error)
}

type ServiceOption func(*Service) error

func WithPersistence(store *Store, reader GenerationReader) ServiceOption {
	return func(service *Service) error {
		if store == nil || reader == nil {
			return errors.New("chat history store and generation reader are required")
		}
		if _, ok := service.rooms.(WorldCatalog); !ok {
			return errors.New("chat room catalog does not expose worlds")
		}
		service.store, service.generations = store, reader
		return nil
	}
}

type SyncResult struct {
	HistoryHealth
	Imported int
	Problems []Problem
	Pending  bool
}

func (s *Service) SyncRoom(ctx context.Context, roomID string) (SyncResult, error) {
	result, _, err := s.syncRoom(ctx, roomID, true)
	return result, err
}

func (s *Service) syncRoom(ctx context.Context, roomID string, wait bool) (SyncResult, bool, error) {
	if s.store == nil || s.generations == nil {
		return SyncResult{}, false, nil
	}
	roomID = strings.TrimSpace(roomID)
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return SyncResult{}, false, err
	}
	if !room.Managed {
		return SyncResult{}, false, ErrRoomNotManaged
	}
	release, acquired, err := s.acquireSync(ctx, roomID, wait)
	if err != nil {
		return SyncResult{}, false, err
	}
	if !acquired {
		return SyncResult{}, false, nil
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	catalog := s.rooms.(WorldCatalog)
	worlds, err := catalog.Worlds(roomID)
	if err != nil {
		return SyncResult{}, true, err
	}
	result := SyncResult{Problems: make([]Problem, 0)}
	budget := syncBudget{bytes: 4 * 1024 * 1024}
	failedWorlds := make(map[string]bool)
phases:
	for _, archives := range []bool{false, true} {
		for _, world := range worlds {
			if err := ctx.Err(); err != nil {
				result.Pending = true
				break phases
			}
			imported, syncErr := s.syncWorld(ctx, roomID, world, &budget, archives)
			result.Imported += imported
			if errors.Is(syncErr, ErrSyncPending) {
				result.Pending = true
				break phases
			}
			if syncErr != nil {
				failedWorlds[world.ID] = true
				result.Problems = append(result.Problems, Problem{
					WorldID: world.ID, WorldName: world.Name, Code: "CHAT_HISTORY_SYNC_FAILED", Message: syncErr.Error(),
				})
			}
		}
	}
	if len(failedWorlds) != 0 && ctx.Err() == nil {
		result.Imported += s.syncCurrentFallback(ctx, room, worlds, failedWorlds)
	}
	health, healthErr := s.store.historyHealth(roomID)
	if healthErr != nil {
		return result, true, healthErr
	}
	if health.PendingGenerations > 0 && len(result.Problems) == 0 {
		result.Pending = true
	}
	now := time.Now().UTC()
	if result.Pending {
		if err := s.store.MarkSync(roomID, "catching_up", "", nil); err != nil {
			return result, true, err
		}
		return result, true, nil
	}
	if len(result.Problems) != 0 {
		message := result.Problems[0].Message
		if len(result.Problems) > 1 {
			message = fmt.Sprintf("%s；另有 %d 个世界同步失败", message, len(result.Problems)-1)
		}
		if err := s.store.MarkSync(roomID, "partial", message, nil); err != nil {
			return result, true, err
		}
		return result, true, nil
	}
	if err := s.store.MarkSync(roomID, "ready", "", &now); err != nil {
		return result, true, err
	}
	return result, true, nil
}

func (s *Service) syncCurrentFallback(ctx context.Context, room rooms.Room, worlds []rooms.World, failed map[string]bool) int {
	snapshot, err := s.logs.RoomChatSnapshot(ctx, room.ID, chatSnapshotTail, "")
	if err != nil {
		return 0
	}
	worldByID := make(map[string]rooms.World, len(worlds))
	for _, world := range worlds {
		worldByID[world.ID] = world
	}
	imported := 0
	for _, source := range snapshot.Worlds {
		world, exists := worldByID[source.WorldID]
		if !exists || !failed[world.ID] || source.Snapshot == nil || source.Problem != nil {
			continue
		}
		generation := shared.RuntimeChatLogGeneration{
			ID:       fallbackGenerationID(room.DirectoryName, world.DirectoryName, source.Snapshot.StartedAt, source.Snapshot.UpdatedAt),
			FileName: source.Snapshot.FileName, Size: source.Snapshot.Size, StartedAt: source.Snapshot.StartedAt,
			StartedAtEstimated: source.Snapshot.StartedAt.IsZero(), UpdatedAt: source.Snapshot.UpdatedAt,
		}
		entries := make([]parsedEntry, 0, len(source.Snapshot.Lines))
		cursor := int64(0)
		for _, line := range source.Snapshot.Lines {
			if line.Cursor > cursor {
				cursor = line.Cursor
			}
			entry, ok := parseLine(room.ID, source, line)
			if ok {
				entry.TimeEstimated = true
				entry.OccurredAt = nil
				entries = append(entries, entry)
			}
		}
		if source.Snapshot.Truncated {
			cursor = 0
		} else if cursor < generation.Size {
			cursor = generation.Size
		}
		count, importErr := s.store.ImportGeneration(room.ID, world, generation, entries, cursor, time.Now().UTC())
		if importErr == nil {
			imported += count
		}
	}
	return imported
}

func fallbackGenerationID(cluster, shard string, startedAt, updatedAt time.Time) string {
	identity := "started:" + startedAt.UTC().Format(time.RFC3339Nano)
	if startedAt.IsZero() {
		identity = "observed:" + updatedAt.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte("chat-v1\x00" + cluster + "\x00" + shard + "\x00" + identity))
	return hex.EncodeToString(sum[:16])
}

type syncBudget struct {
	bytes  int64
	chunks int
}

func (s *Service) syncWorld(ctx context.Context, roomID string, world rooms.World, budget *syncBudget, archives bool) (int, error) {
	generations, err := s.generations.ListChatLogGenerations(ctx, roomID, world.ID)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(generations))
	for _, generation := range generations {
		ids = append(ids, generation.ID)
	}
	if err := s.store.observeGenerations(roomID, world.ID, ids); err != nil {
		return 0, err
	}
	sort.SliceStable(generations, func(i, j int) bool {
		if generations[i].Archived != generations[j].Archived {
			return !generations[i].Archived
		}
		return generations[i].StartedAt.After(generations[j].StartedAt)
	})
	imported := 0
	for _, generation := range generations {
		if generation.Archived != archives {
			continue
		}
		if ctx.Err() != nil {
			return imported, ErrSyncPending
		}
		state, err := s.store.Generation(roomID, world.ID, generation.ID)
		if err != nil {
			return imported, err
		}
		repair := state.ParserVersion < parserVersion
		// Fresh appends precede old replay within the current process generation.
		if !generation.Archived && state.Cursor > 0 && state.Cursor < generation.Size {
			repair = false
		}
		if !repair && state.Cursor == generation.Size && state.CaughtUp {
			continue
		}
		if state == (GenerationState{}) {
			if err := s.store.ReconcileGeneration(roomID, world, generation); err != nil {
				return imported, err
			}
		}
		cursor := state.Cursor
		if repair {
			cursor = state.RepairCursor
		}
		if cursor > generation.Size {
			return imported, errors.New("聊天日志被截短，保留已保存记录并等待核验")
		}
		if generation.Size == 0 {
			_, err := s.store.importGeneration(roomID, world, generation, nil, 0, time.Now().UTC(), batchProgress{version: parserVersion, repair: repair})
			if err != nil {
				return imported, err
			}
			continue
		}
		for {
			if ctx.Err() != nil || budget.bytes < 2 || budget.chunks >= maximumSyncChunks {
				return imported, ErrSyncPending
			}
			maxBytes := int(min(int64(chatSyncChunkBytes), budget.bytes/2))
			chunk, err := s.generations.ReadChatLogGeneration(ctx, roomID, world.ID, shared.RuntimeChatLogRequest{
				GenerationID: generation.ID, Cursor: cursor, MaxBytes: maxBytes, MaxLines: chatSyncChunkLines,
				ResolveTimes: generation.TimeVersion >= shared.ChatTimeVersion,
			})
			budget.chunks++
			if err != nil {
				if ctx.Err() != nil {
					return imported, ErrSyncPending
				}
				return imported, err
			}
			budget.bytes -= max(chunk.ReadBytes, chunk.Cursor-cursor)
			if chunk.Generation == nil || chunk.Cursor < cursor {
				return imported, errors.New("聊天日志节点返回了无效游标")
			}
			if chunk.TimeVersion == shared.ChatTimeVersion && !chunk.TimesReady {
				if chunk.ReadBytes == 0 {
					return imported, ErrSyncPending
				}
				continue
			}
			parsed := make([]parsedEntry, 0, len(chunk.Lines))
			progress := batchProgress{version: parserVersion, repair: repair}
			worldSnapshot := logstream.WorldSnapshot{WorldID: world.ID, WorldName: world.Name, WorldRole: world.Role}
			times := make(map[int64]*time.Time, len(chunk.Times))
			for _, stamp := range chunk.Times {
				times[stamp.Cursor] = stamp.OccurredAt
			}
			for _, line := range chunk.Lines {
				entry, ok := parseLine(roomID, worldSnapshot, logstream.Line{Cursor: line.Cursor, Text: line.Text})
				if !ok {
					if strings.TrimSpace(line.Text) != "" && !strings.Contains(line.Text, "]: [Mod Warning]") {
						progress.parseErrors++
						progress.lastParseProblem = fmt.Sprintf("字节 %d：无法识别聊天行", line.Cursor)
					}
					continue
				}
				entry.timeVersion = parserVersion
				entry.OccurredAt = times[line.Cursor]
				entry.TimeEstimated = entry.OccurredAt == nil
				parsed = append(parsed, entry)
			}
			count, err := s.store.importGeneration(roomID, world, *chunk.Generation, parsed, chunk.Cursor, time.Now().UTC(), progress)
			imported += count
			if err != nil {
				return imported, err
			}
			if chunk.Complete {
				break
			}
			if chunk.Cursor == cursor {
				return imported, ErrSyncPending
			}
			cursor = chunk.Cursor
		}
	}
	return imported, nil
}

func (s *Service) acquireSync(ctx context.Context, key string, wait bool) (func(), bool, error) {
	s.syncLocksMu.Lock()
	gate := s.syncLocks[key]
	if s.syncLocks[key] == nil {
		gate = make(chan struct{}, 1)
		s.syncLocks[key] = gate
	}
	s.syncLocksMu.Unlock()
	if !wait {
		select {
		case gate <- struct{}{}:
			return func() { <-gate }, true, nil
		default:
			return nil, false, nil
		}
	}
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}
