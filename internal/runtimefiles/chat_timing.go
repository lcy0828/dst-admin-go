package runtimefiles

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/dsttime"
)

const maximumChatClockRows = 50000

var chatClockPattern = regexp.MustCompile(`^\[(\d{2,}):([0-5]\d):([0-5]\d)\]:\s*\[([^\]]+)\]\s*(.*)$`)

type chatClockRow struct {
	cursor      int64
	seconds     int64
	label, name string
}
type chatClockAnchor struct {
	name, kind string
	at         time.Time
}
type chatClockFile struct {
	identity string
	cursor   int64
	pending  string
	clock    dsttime.LogClock
	anchors  []chatClockAnchor
	names    map[string]string
	rows     []chatClockRow
	accessed time.Time
	failure  error
}

// No background work: bounded, incremental indexes are populated by reads.
// 64 files * 50k bounded metadata rows is the absolute cache ceiling.
var chatClockCache = struct {
	sync.Mutex
	files map[string]*chatClockFile
}{files: make(map[string]*chatClockFile)}

func resolveChatLogTimes(ctx context.Context, selected *chatLogCandidate, budget int64) (map[int64]*time.Time, bool, int64, error) {
	chatClockCache.Lock()
	defer chatClockCache.Unlock()
	initial := budget
	chat, ready, err := scanChatClockFile(ctx, selected.path, selected.generation.ID, true, selected.generation.Archived, &budget)
	if err != nil || !ready {
		return nil, ready, initial - budget, err
	}
	start := selected.generation.StartedAt
	end := selected.generation.UpdatedAt
	var anchors []chatClockAnchor
	if len(chat.rows) > 0 && !start.IsZero() && !selected.generation.StartedAtEstimated && end.Sub(start) >= 24*time.Hour {
		paths, err := chatClockServerPaths(selected, start, end)
		if err != nil {
			return nil, false, initial - budget, err
		}
		for _, path := range paths {
			index, complete, err := scanChatClockFile(ctx, path, "server", false, filepath.Base(path) != "server_log.txt" && filepath.Base(path) != "forest_server_log.txt", &budget)
			if err != nil || !complete {
				return nil, false, initial - budget, err
			}
			anchors = append(anchors, index.anchors...)
		}
	}
	if selected.generation.StartedAtEstimated {
		start = time.Time{}
	}
	return composeChatTimes(chat.rows, start, end, anchors), true, initial - budget, nil
}

func scanChatClockFile(ctx context.Context, path, generation string, chat, final bool, budget *int64) (*chatClockFile, bool, error) {
	info, exists, err := trustedChatLogInfo(path)
	if err != nil || !exists {
		return nil, false, errors.Join(err, os.ErrNotExist)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	identity, err := runtimeFileID(file, filepath.Base(path))
	if err != nil {
		return nil, false, err
	}
	identity += ":" + generation
	if !chat {
		identity += ":" + readLogStartTime(file).Format(time.RFC3339Nano)
	}
	state := chatClockCache.files[path]
	if state == nil || state.identity != identity || info.Size() < state.cursor {
		if len(chatClockCache.files) >= 64 {
			oldest := ""
			for key, entry := range chatClockCache.files {
				if oldest == "" || entry.accessed.Before(chatClockCache.files[oldest].accessed) {
					oldest = key
				}
			}
			delete(chatClockCache.files, oldest)
		}
		state = &chatClockFile{identity: identity, names: make(map[string]string)}
		chatClockCache.files[path] = state
	}
	state.accessed = time.Now()
	if state.failure != nil {
		return state, false, state.failure
	}
	remaining := min(info.Size()-state.cursor, *budget)
	reader := bufio.NewReader(io.NewSectionReader(file, state.cursor, remaining))
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return state, false, err
		}
		part, readErr := reader.ReadString('\n')
		state.cursor += int64(len(part))
		*budget -= int64(len(part))
		remaining -= int64(len(part))
		state.pending += part
		if len(state.pending) > 1024*1024 {
			state.failure = errors.New("chat clock line exceeds 1 MiB")
			return state, false, state.failure
		}
		if strings.HasSuffix(state.pending, "\n") || final && state.cursor == info.Size() && state.pending != "" {
			line := strings.TrimSpace(state.pending)
			state.pending = ""
			if chat {
				if row, ok := parseChatClockRow(state.cursor, line); ok {
					state.rows = append(state.rows, row)
				}
			} else {
				at, payload := state.clock.ReadLine(line)
				if !at.IsZero() {
					if match := historyAuthenticated.FindStringSubmatch(payload); len(match) > 0 {
						state.names[match[1]] = match[2]
						state.anchors = append(state.anchors, chatClockAnchor{name: match[2], kind: "join", at: at})
					} else if match := historyDisconnected.FindStringSubmatch(payload); len(match) > 0 && state.names[match[1]] != "" {
						state.anchors = append(state.anchors, chatClockAnchor{name: state.names[match[1]], kind: "leave", at: at})
					}
				}
			}
			if len(state.rows)+len(state.anchors)+len(state.names) > maximumChatClockRows {
				state.failure = errors.New("chat clock index exceeds its bounded capacity")
				return state, false, state.failure
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return state, false, readErr
		}
		if len(part) == 0 {
			break
		}
	}
	return state, state.cursor == info.Size(), nil
}

func parseChatClockRow(cursor int64, line string) (chatClockRow, bool) {
	m := chatClockPattern.FindStringSubmatch(line)
	if m == nil {
		return chatClockRow{}, false
	}
	h, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil || h > 1000000 {
		return chatClockRow{}, false
	}
	minutes, _ := strconv.ParseInt(m[2], 10, 64)
	seconds, _ := strconv.ParseInt(m[3], 10, 64)
	row := chatClockRow{cursor: cursor, seconds: h*3600 + minutes*60 + seconds, label: strings.ToLower(m[4])}
	if row.label == "join announcement" || row.label == "leave announcement" {
		row.name = strings.TrimSpace(m[5])
	}
	return row, true
}

// Nearby sibling shard logs can supply cluster join/leave evidence even when
// this shard only receives the chat broadcast. A split deployment simply has
// fewer anchors and leaves ambiguous dates unconfirmed.
func chatClockServerPaths(selected *chatLogCandidate, start, end time.Time) ([]string, error) {
	worldRoot := filepath.Dir(selected.path)
	if selected.generation.Archived {
		worldRoot = filepath.Dir(filepath.Dir(worldRoot))
	}
	clusterRoot := filepath.Dir(worldRoot)
	entries, err := trustedDirectoryEntries(clusterRoot)
	if err != nil {
		return nil, err
	}
	byBoot := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(clusterRoot, entry.Name())
		archives, err := archivedServerLogCandidates(root)
		if err != nil {
			return nil, err
		}
		paths := []string{filepath.Join(root, "server_log.txt"), filepath.Join(root, "forest_server_log.txt")}
		for _, archive := range archives {
			if !archive.rotatedAt.Before(start) {
				paths = append(paths, archive.path)
			}
		}
		for _, path := range paths {
			info, exists, err := trustedChatLogInfo(path)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			boot := time.Time{}
			if filepath.Base(path) == "server_log.txt" || filepath.Base(path) == "forest_server_log.txt" {
				boot = trustedLogStartTime(path)
			} else {
				boot = cachedLogStartTime(path)
			}
			if boot.IsZero() || boot.After(end) || info.ModTime().Before(start) {
				continue
			}
			key := root + ":" + boot.Format(time.RFC3339Nano)
			previous := byBoot[key]
			if previous != "" {
				old, err := os.Stat(previous)
				if err == nil && old.Size() >= info.Size() {
					continue
				}
			}
			byBoot[key] = path
		}
	}
	if len(byBoot) > 32 {
		return nil, errors.New("chat clock overlap exceeds 32 server generations")
	}
	paths := make([]string, 0, len(byBoot))
	for _, path := range byBoot {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func composeChatTimes(rows []chatClockRow, start, end time.Time, anchors []chatClockAnchor) map[int64]*time.Time {
	result := make(map[int64]*time.Time, len(rows))
	if start.IsZero() || end.Before(start) {
		return result
	}
	type bounds struct {
		low, high int64
		choices   []int64
	}
	ranges := make([]bounds, len(rows))
	byName := make(map[string][]chatClockAnchor)
	for _, anchor := range anchors {
		byName[anchor.kind+"\x00"+anchor.name] = append(byName[anchor.kind+"\x00"+anchor.name], anchor)
	}
	for i, row := range rows {
		maxSeconds := int64(end.Sub(start).Seconds())
		high := int64(-1)
		if maxSeconds >= row.seconds {
			high = (maxSeconds - row.seconds) / 86400
		}
		if row.seconds >= 86400 && high >= 0 {
			high = 0
		}
		ranges[i] = bounds{high: high}
		if row.name != "" {
			kind := strings.TrimSuffix(row.label, " announcement")
			seen := make(map[int64]bool)
			for _, anchor := range byName[kind+"\x00"+row.name] {
				elapsed := int64(anchor.at.Sub(start).Seconds())
				day := (elapsed - row.seconds + 43200) / 86400
				if day < 0 || day > high {
					continue
				}
				delta := start.Add(time.Duration(day*86400+row.seconds) * time.Second).Sub(anchor.at)
				if kind == "join" && delta >= -2*time.Second && delta <= 5*time.Minute || kind == "leave" && delta >= -2*time.Second && delta <= 2*time.Second {
					seen[day] = true
				}
			}
			for day := range seen {
				ranges[i].choices = append(ranges[i].choices, day)
			}
			sort.Slice(ranges[i].choices, func(a, b int) bool { return ranges[i].choices[a] < ranges[i].choices[b] })
			if len(ranges[i].choices) > 0 {
				ranges[i].low = ranges[i].choices[0]
				ranges[i].high = ranges[i].choices[len(ranges[i].choices)-1]
			}
		}
	}
	for i := range rows {
		b := &ranges[i]
		if i > 0 {
			minimum := ranges[i-1].low*86400 + rows[i-1].seconds - 2
			for b.low*86400+rows[i].seconds < minimum {
				b.low++
			}
		}
		if len(b.choices) > 0 {
			pos := sort.Search(len(b.choices), func(j int) bool { return b.choices[j] >= b.low })
			if pos == len(b.choices) {
				return result
			}
			b.low = b.choices[pos]
		}
		if b.low > b.high {
			return result
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		b := &ranges[i]
		if i < len(rows)-1 {
			maximum := ranges[i+1].high*86400 + rows[i+1].seconds + 2
			for b.high*86400+rows[i].seconds > maximum {
				b.high--
			}
		}
		if len(b.choices) > 0 {
			pos := sort.Search(len(b.choices), func(j int) bool { return b.choices[j] > b.high }) - 1
			if pos < 0 {
				return result
			}
			b.high = b.choices[pos]
		}
		if b.low > b.high {
			return result
		}
	}
	for i, row := range rows {
		if ranges[i].low == ranges[i].high {
			at := start.Add(time.Duration(ranges[i].low*86400+row.seconds) * time.Second).UTC()
			result[row.cursor] = &at
		}
	}
	return result
}
