package runtimefiles

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/dsttime"
	"dont/shared"
)

const MaximumHistoricalPlayers = 4096
const playerHistoryReadBudget = 16 * 1024 * 1024

// PlayerHistoryEntry contains event times, never the time a log was imported.
type PlayerHistoryEntry struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Prefab             string    `json:"prefab,omitempty"`
	NetID              string    `json:"netId,omitempty"`
	FirstSeenAt        time.Time `json:"firstSeenAt"`
	LastSeenAt         time.Time `json:"lastSeenAt"`
	LastSeenWorld      string    `json:"lastSeenWorld"`
	LastConnectedAt    time.Time `json:"lastConnectedAt"`
	LastDisconnectedAt time.Time `json:"lastDisconnectedAt"`
	PrefabObservedAt   time.Time `json:"prefabObservedAt"`
}

type PlayerHistoryResult struct {
	Players  []PlayerHistoryEntry `json:"players"`
	Complete bool                 `json:"complete"`
}

type playerHistoryLog struct {
	identity string
	cursor   int64
	pending  string
	clock    dsttime.LogClock
}

type playerHistoryWorld struct {
	logs     map[string]*playerHistoryLog
	players  map[string]PlayerHistoryEntry
	accessed time.Time
}

var playerHistoryCache = struct {
	sync.Mutex
	worlds map[string]*playerHistoryWorld
}{worlds: make(map[string]*playerHistoryWorld)}

var historyAuthenticated = regexp.MustCompile(`^Client authenticated: \(([A-Za-z0-9_-]{1,128})\) (.+)$`)
var historyOwnership = regexp.MustCompile(`^User ID\s+([A-Za-z0-9_-]{1,128})\s+assigned ownership to entity\s+\d+ - ([A-Za-z0-9_-]{1,128})\s*$`)
var historyDisconnected = regexp.MustCompile(`^\[Shard\] \(([A-Za-z0-9_-]{1,128})\) disconnected from (.+)\([^()]+\)\s*$`)
var historyIdentity = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func readPlayerHistoryArtifact(ctx context.Context, root, shard string) (shared.RuntimeArtifactBundle, error) {
	result, err := readPlayerHistory(ctx, root, shard)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return shared.RuntimeArtifactBundle{}, err
	}
	if int64(len(data)) > MaximumBundleBytes {
		return shared.RuntimeArtifactBundle{}, errors.New("player history exceeds 4 MiB")
	}
	sum := sha256.Sum256(data)
	return shared.RuntimeArtifactBundle{Kind: shared.ArtifactRuntimePlayerHistory, Artifacts: []shared.RuntimeArtifact{{
		Name: "player-history.json", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), UpdatedAt: time.Now().UTC(), Data: data,
	}}}, nil
}

func readPlayerHistory(ctx context.Context, root, shard string) (PlayerHistoryResult, error) {
	archives, err := archivedServerLogCandidates(root)
	if err != nil {
		return PlayerHistoryResult{}, err
	}
	if len(archives) > 1024 {
		return PlayerHistoryResult{}, errors.New("player history exceeds 1024 archived logs")
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].rotatedAt.After(archives[j].rotatedAt) })
	// Read the current generation first so catching up archives cannot delay
	// recent short connections. Unchanged files only require stat/header reads.
	paths := []string{filepath.Join(root, "server_log.txt"), filepath.Join(root, "forest_server_log.txt")}
	for _, archive := range archives {
		paths = append(paths, archive.path)
	}
	playerHistoryCache.Lock()
	defer playerHistoryCache.Unlock()
	world := playerHistoryCache.worlds[root]
	if world == nil {
		if len(playerHistoryCache.worlds) >= 32 {
			oldest := ""
			for key, entry := range playerHistoryCache.worlds {
				if oldest == "" || entry.accessed.Before(playerHistoryCache.worlds[oldest].accessed) {
					oldest = key
				}
			}
			delete(playerHistoryCache.worlds, oldest)
		}
		world = &playerHistoryWorld{logs: make(map[string]*playerHistoryLog), players: make(map[string]PlayerHistoryEntry)}
		playerHistoryCache.worlds[root] = world
	}
	world.accessed = time.Now()
	budget := int64(playerHistoryReadBudget)
	result := PlayerHistoryResult{Complete: true, Players: []PlayerHistoryEntry{}}
	active := make(map[string]bool)
	for _, path := range paths {
		active[path] = true
		if err := ctx.Err(); err != nil {
			return PlayerHistoryResult{}, err
		}
		complete, err := world.readLog(ctx, path, shard, &budget)
		if err != nil {
			return PlayerHistoryResult{}, err
		}
		result.Complete = result.Complete && complete
	}
	for path := range world.logs {
		if !active[path] {
			delete(world.logs, path)
		}
	}
	for _, player := range world.players {
		result.Players = append(result.Players, player)
	}
	sort.Slice(result.Players, func(i, j int) bool { return result.Players[i].ID < result.Players[j].ID })
	return result, nil
}

func (w *playerHistoryWorld) readLog(ctx context.Context, path, shard string, budget *int64) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("unsafe player history log")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	identity, err := runtimeFileID(file, filepath.Base(path))
	if err != nil {
		return false, err
	}
	// DST can truncate and regrow the same inode between two reads. Its first
	// line only names PersistRootStorage, so also include the boot timestamp.
	identity += ":" + readLogStartTime(file).Format(time.RFC3339Nano)
	state := w.logs[path]
	if state == nil || state.identity != identity || info.Size() < state.cursor {
		state = &playerHistoryLog{identity: identity}
		w.logs[path] = state
	}
	remaining := info.Size() - state.cursor
	if remaining == 0 {
		return true, nil
	}
	if *budget <= 0 {
		return false, nil
	}
	if remaining > *budget {
		remaining = *budget
	}
	reader := bufio.NewReader(io.NewSectionReader(file, state.cursor, remaining))
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		part, readErr := reader.ReadString('\n')
		state.cursor += int64(len(part))
		*budget -= int64(len(part))
		state.pending += part
		if len(state.pending) > 1024*1024 {
			return false, errors.New("player history line exceeds 1 MiB")
		}
		if strings.HasSuffix(state.pending, "\n") || readErr == io.EOF && state.cursor == info.Size() && filepath.Base(path) != "server_log.txt" && filepath.Base(path) != "forest_server_log.txt" {
			line := strings.TrimRight(state.pending, "\r\n")
			state.pending = ""
			if err := w.applyLine(&state.clock, shard, line); err != nil {
				return false, err
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return false, readErr
			}
			break
		}
	}
	return state.cursor == info.Size(), nil
}

func (w *playerHistoryWorld) applyLine(clock *dsttime.LogClock, shard, line string) error {
	at, payload := clock.ReadLine(line)
	if at.IsZero() || at.After(time.Now().Add(30*time.Second)) {
		return nil
	}
	var id, name, prefab, netID string
	eventWorld := shard
	joined, left := false, false
	if match := historyAuthenticated.FindStringSubmatch(payload); match != nil {
		id, name, joined = match[1], strings.TrimSpace(match[2]), true
	} else if match := historyOwnership.FindStringSubmatch(payload); match != nil {
		id, prefab = match[1], match[2]
	} else if match := historyDisconnected.FindStringSubmatch(payload); match != nil {
		id, left = match[1], true
		eventWorld = match[2]
	} else if strings.HasPrefix(payload, "[ClientObject] Initialized (authenticated) on server:") {
		for _, field := range strings.Fields(payload) {
			key, value, _ := strings.Cut(field, "=")
			if key == "userid" {
				id = value
			}
			if key == "netid" {
				netID = value
			}
		}
	} else {
		return nil
	}
	if !historyIdentity.MatchString(id) || len([]rune(name)) > 256 || len(netID) > 128 {
		return nil
	}
	p, exists := w.players[id]
	if !exists && len(w.players) >= MaximumHistoricalPlayers {
		return errors.New("player history exceeds 4096 identities")
	}
	p.ID = id
	if p.Name == "" {
		p.Name = id
	}
	if p.FirstSeenAt.IsZero() || at.Before(p.FirstSeenAt) {
		p.FirstSeenAt = at
	}
	if !at.Before(p.LastSeenAt) {
		p.LastSeenAt = at
		p.LastSeenWorld = eventWorld
		if name != "" {
			p.Name = name
		}
		if netID != "" {
			p.NetID = netID
		}
	} else {
		if p.Name == id && name != "" {
			p.Name = name
		}
		if p.NetID == "" && netID != "" {
			p.NetID = netID
		}
	}
	if joined && at.After(p.LastConnectedAt) {
		p.LastConnectedAt = at
	}
	if left && at.After(p.LastDisconnectedAt) {
		p.LastDisconnectedAt = at
	}
	if prefab != "" && at.After(p.PrefabObservedAt) {
		p.Prefab, p.PrefabObservedAt = prefab, at
	}
	w.players[id] = p
	return nil
}
