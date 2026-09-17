package chatlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dont/internal/dsttime"
	"dont/internal/logstream"
	"dont/internal/rooms"
)

const (
	defaultLimit     = 100
	maximumLimit     = 500
	chatSnapshotTail = 2000
	dedupWindow      = 10
)

var chatLinePattern = regexp.MustCompile(`^\[(\d{2,}):(\d{2}):(\d{2})\]:\s+\[([^\]]+)\]\s*(.*)$`)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
}

type SnapshotReader interface {
	RoomChatSnapshot(context.Context, string, int, string) (logstream.RoomSnapshot, error)
}

type Service struct {
	rooms       RoomCatalog
	logs        SnapshotReader
	store       *Store
	generations GenerationReader
	syncLocksMu sync.Mutex
	syncLocks   map[string]chan struct{}
	attemptsMu  sync.Mutex
	attempts    map[string]time.Time
}

type parsedEntry struct {
	Entry
	seconds      int
	sourceCursor int64
	primaryRole  rooms.WorldRole
	timeVersion  int
}

func NewService(roomCatalog RoomCatalog, logs SnapshotReader, options ...ServiceOption) (*Service, error) {
	if roomCatalog == nil || logs == nil {
		return nil, errors.New("rooms and chat logs are required")
	}
	service := &Service{
		rooms: roomCatalog, logs: logs, syncLocks: make(map[string]chan struct{}), attempts: make(map[string]time.Time),
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("chat log service option is required")
		}
		if err := option(service); err != nil {
			return nil, err
		}
	}
	return service, nil
}

func (s *Service) List(ctx context.Context, roomID string, filter Filter) (List, error) {
	room, err := s.rooms.Room(strings.TrimSpace(roomID))
	if err != nil {
		return List{}, err
	}
	if !room.Managed {
		return List{}, ErrRoomNotManaged
	}
	filter, err = normalizeFilter(filter)
	if err != nil {
		return List{}, err
	}
	if s.store != nil {
		if filter.Offset == 0 && s.shouldSync(room.ID, time.Now()) {
			syncContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, _, _ = s.syncRoom(syncContext, room.ID, false)
			cancel()
		}
		return s.store.List(room.ID, filter)
	}
	snapshot, err := s.logs.RoomChatSnapshot(ctx, room.ID, chatSnapshotTail, "")
	if err != nil {
		return List{}, err
	}

	entries := make([]parsedEntry, 0)
	problems := make([]Problem, 0, snapshot.Unavailable)
	truncated := false
	var startedAt time.Time
	startedRolePriority := int(^uint(0) >> 1)
	var updatedAt time.Time
	for _, world := range snapshot.Worlds {
		if world.Problem != nil {
			problems = append(problems, Problem{
				WorldID: world.WorldID, WorldName: world.WorldName,
				Code: world.Problem.Code, Message: world.Problem.Message,
			})
			continue
		}
		if world.Snapshot == nil {
			continue
		}
		if world.Snapshot.UpdatedAt.After(updatedAt) {
			updatedAt = world.Snapshot.UpdatedAt
		}
		if !world.Snapshot.StartedAt.IsZero() && sourcePriority(world.WorldRole) < startedRolePriority {
			startedAt = world.Snapshot.StartedAt
			startedRolePriority = sourcePriority(world.WorldRole)
		}
		truncated = truncated || world.Snapshot.Truncated
		for _, line := range world.Snapshot.Lines {
			entry, ok := parseLine(room.ID, world, line)
			if ok {
				entries = append(entries, entry)
			}
		}
	}

	entries = deduplicate(entries)
	counts := map[Kind]int{KindSay: 0, KindWhisper: 0, KindAnnouncement: 0}
	filtered := make([]parsedEntry, 0, len(entries))
	query := strings.ToLower(filter.Query)
	for _, entry := range entries {
		if filter.WorldID != "" && !hasSource(entry.Sources, filter.WorldID) {
			continue
		}
		if query != "" && !entryContains(entry, query) {
			continue
		}
		counts[entry.Kind]++
		if filter.Kind == "" || entry.Kind == filter.Kind {
			filtered = append(filtered, entry)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].OccurredAt != nil && filtered[j].OccurredAt != nil && !filtered[i].OccurredAt.Equal(*filtered[j].OccurredAt) {
			return filtered[i].OccurredAt.After(*filtered[j].OccurredAt)
		}
		if filtered[i].seconds != filtered[j].seconds {
			return filtered[i].seconds > filtered[j].seconds
		}
		return filtered[i].ID > filtered[j].ID
	})
	total := len(filtered)
	start := filter.Offset
	if start > total {
		start = total
	}
	end := start + filter.Limit
	if end > total {
		end = total
	}
	items := make([]Entry, 0, end-start)
	for _, entry := range filtered[start:end] {
		items = append(items, entry.Entry)
	}
	var updated *time.Time
	if snapshot.Available > 0 && !updatedAt.IsZero() {
		value := updatedAt.UTC()
		updated = &value
	}
	var started *time.Time
	if !startedAt.IsZero() {
		value := startedAt.UTC()
		started = &value
	}
	return List{
		Items: items, Total: total, Counts: counts, Limit: filter.Limit, Offset: filter.Offset,
		Partial: snapshot.Partial, Truncated: truncated, AvailableWorlds: snapshot.Available,
		UnavailableWorlds: snapshot.Unavailable, StartedAt: started, UpdatedAt: updated, Problems: problems,
	}, nil
}

func (s *Service) shouldSync(roomID string, now time.Time) bool {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()
	if previous := s.attempts[roomID]; !previous.IsZero() && now.Sub(previous) < 2*time.Second {
		return false
	}
	s.attempts[roomID] = now
	return true
}

func normalizeFilter(filter Filter) (Filter, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.WorldID = strings.TrimSpace(filter.WorldID)
	if !utf8.ValidString(filter.Query) || len([]rune(filter.Query)) > 256 || filter.Offset < 0 ||
		filter.Kind != "" && filter.Kind != KindSay && filter.Kind != KindWhisper && filter.Kind != KindAnnouncement {
		return Filter{}, ErrInvalidFilter
	}
	if filter.Limit == 0 {
		filter.Limit = defaultLimit
	}
	if filter.Limit < 1 || filter.Limit > maximumLimit {
		return Filter{}, ErrInvalidFilter
	}
	return filter, nil
}

func parseLine(roomID string, world logstream.WorldSnapshot, line logstream.Line) (parsedEntry, bool) {
	matches := chatLinePattern.FindStringSubmatch(strings.TrimSpace(line.Text))
	if len(matches) != 6 {
		return parsedEntry{}, false
	}
	hour, _ := strconv.Atoi(matches[1])
	minute, _ := strconv.Atoi(matches[2])
	second, _ := strconv.Atoi(matches[3])
	seconds := hour*3600 + minute*60 + second
	label := strings.TrimSpace(matches[4])
	payload := strings.TrimSpace(matches[5])
	entry := parsedEntry{
		Entry: Entry{
			Content: payload, SourceTimestamp: fmt.Sprintf("%02d:%02d:%02d", hour, minute, second),
			Sources: []Source{{WorldID: world.WorldID, WorldName: world.WorldName, WorldRole: world.WorldRole}},
		},
		seconds: seconds, sourceCursor: line.Cursor, primaryRole: world.WorldRole,
	}
	if world.Snapshot != nil {
		if occurredAt, err := dsttime.ResolveTimestamp(world.Snapshot.StartedAt, entry.SourceTimestamp); err == nil {
			value := occurredAt.UTC()
			entry.OccurredAt = &value
		}
	}
	switch strings.ToLower(label) {
	case "say":
		entry.Kind = KindSay
	case "whisper":
		entry.Kind = KindWhisper
	case "announcement":
		entry.Kind = KindAnnouncement
	default:
		lower := strings.ToLower(label)
		if !strings.HasSuffix(lower, " announcement") {
			return parsedEntry{}, false
		}
		entry.Kind = KindAnnouncement
		entry.AnnouncementType = normalizeAnnouncementType(strings.TrimSuffix(lower, " announcement"))
	}
	if entry.Kind == KindSay || entry.Kind == KindWhisper {
		playerID, playerName, content, ok := parsePlayerMessage(payload)
		if !ok {
			return parsedEntry{}, false
		}
		entry.PlayerID, entry.PlayerName, entry.Content = playerID, playerName, content
	}
	entry.ID = entryID(roomID, entry)
	return entry, true
}

func parsePlayerMessage(value string) (string, string, string, bool) {
	if !strings.HasPrefix(value, "(") {
		return "", "", "", false
	}
	closing := strings.IndexByte(value, ')')
	if closing < 2 {
		return "", "", "", false
	}
	playerID := strings.TrimSpace(value[1:closing])
	remainder := strings.TrimSpace(value[closing+1:])
	separator := strings.Index(remainder, ":")
	if playerID == "" || separator < 1 {
		return "", "", "", false
	}
	playerName := strings.TrimSpace(remainder[:separator])
	content := strings.TrimSpace(remainder[separator+1:])
	if playerName == "" {
		return "", "", "", false
	}
	return playerID, playerName, content, true
}

func normalizeAnnouncementType(value string) string {
	return strings.Join(strings.Fields(value), "-")
}

func deduplicate(entries []parsedEntry) []parsedEntry {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].OccurredAt != nil && entries[j].OccurredAt != nil && !entries[i].OccurredAt.Equal(*entries[j].OccurredAt) {
			return entries[i].OccurredAt.Before(*entries[j].OccurredAt)
		}
		if entries[i].seconds != entries[j].seconds {
			return entries[i].seconds < entries[j].seconds
		}
		return sourcePriority(entries[i].primaryRole) < sourcePriority(entries[j].primaryRole)
	})
	unique := make([]parsedEntry, 0, len(entries))
	groups := make(map[string][]int)
	for _, entry := range entries {
		identity := entryIdentity(entry)
		matched := -1
		indices := groups[identity]
		for index := len(indices) - 1; index >= 0; index-- {
			candidateIndex := indices[index]
			candidate := unique[candidateIndex]
			if withinDedupWindow(candidate, entry) && !hasSource(candidate.Sources, entry.Sources[0].WorldID) {
				matched = candidateIndex
				break
			}
		}
		if matched < 0 {
			unique = append(unique, entry)
			groups[identity] = append(groups[identity], len(unique)-1)
			continue
		}
		candidate := &unique[matched]
		sources := append(candidate.Sources, entry.Sources[0])
		sortSources(sources)
		if sourcePriority(entry.primaryRole) < sourcePriority(candidate.primaryRole) {
			entry.Sources = sources
			*candidate = entry
		} else {
			candidate.Sources = sources
		}
	}
	return unique
}

func withinDedupWindow(left, right parsedEntry) bool {
	if left.OccurredAt != nil && right.OccurredAt != nil {
		delta := right.OccurredAt.Sub(*left.OccurredAt)
		if delta < 0 {
			delta = -delta
		}
		return delta <= time.Duration(dedupWindow)*time.Second
	}
	delta := right.seconds - left.seconds
	if delta < 0 {
		delta = -delta
	}
	return delta <= dedupWindow
}

func entryIdentity(entry parsedEntry) string {
	return strings.Join([]string{
		string(entry.Kind), entry.AnnouncementType, entry.PlayerID,
		strings.ToLower(entry.PlayerName), entry.Content,
	}, "\x00")
}

func entryID(roomID string, entry parsedEntry) string {
	value := roomID + "\x00" + entry.SourceTimestamp + "\x00" + entryIdentity(entry)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func hasSource(sources []Source, worldID string) bool {
	for _, source := range sources {
		if source.WorldID == worldID {
			return true
		}
	}
	return false
}

func sortSources(sources []Source) {
	sort.SliceStable(sources, func(i, j int) bool {
		return sourcePriority(sources[i].WorldRole) < sourcePriority(sources[j].WorldRole)
	})
}

func sourcePriority(role rooms.WorldRole) int {
	switch role {
	case rooms.WorldRoleMaster:
		return 0
	case rooms.WorldRoleCaves:
		return 1
	default:
		return 2
	}
}

func entryContains(entry parsedEntry, query string) bool {
	values := []string{entry.PlayerID, entry.PlayerName, entry.Content, entry.AnnouncementType}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}
