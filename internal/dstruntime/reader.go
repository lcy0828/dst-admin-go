package dstruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/rooms"
)

const (
	maxSnapshotBytes = int64(1024 * 1024)
	maxHealthBytes   = int64(64 * 1024)
	defaultFreshFor  = 20 * time.Second
	maxFutureSkew    = 30 * time.Second
)

var snapshotPlayerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (m *Manager) ReadPlayers(ctx context.Context, roomID, worldID string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return Snapshot{}, err
	}
	if !room.Managed {
		return Snapshot{}, rooms.ErrRoomNotManaged
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return Snapshot{}, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return Snapshot{}, err
	}
	status := m.inspectAt(room, world, worldPath)
	if status.State == InstallStateMissing {
		return Snapshot{}, ErrRuntimeNotInstalled
	}
	if status.State != InstallStateInstalled {
		return Snapshot{}, fmt.Errorf("%w: %s", ErrRuntimeNotInstalled, status.Message)
	}
	expectedSession := currentSessionID(worldPath)
	candidates := make([]Snapshot, 0, 2)
	var failures error
	for _, name := range []string{"players-a.json", "players-b.json"} {
		value, readErr := readSnapshot(filepath.Join(worldPath, "save", "mod_config_data", "dst-admin", name), expectedSession, m.now().UTC())
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				failures = errors.Join(failures, fmt.Errorf("%s: %w", name, readErr))
			}
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		if failures != nil {
			return Snapshot{}, fmt.Errorf("%w: %v", ErrSnapshotUnavailable, failures)
		}
		return Snapshot{}, ErrSnapshotUnavailable
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.ProducerInstanceID == right.ProducerInstanceID && left.Sequence != right.Sequence {
			return left.Sequence > right.Sequence
		}
		if !left.CapturedAt.Equal(right.CapturedAt) {
			return left.CapturedAt.After(right.CapturedAt)
		}
		return left.Sequence > right.Sequence
	})
	return candidates[0], nil
}

func (m *Manager) Health(roomID, worldID string) (Health, error) {
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return Health{}, err
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return Health{}, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return Health{}, err
	}
	data, _, exists, err := readRegular(filepath.Join(worldPath, "save", "mod_config_data", "dst-admin", "health.json"), maxHealthBytes)
	if err != nil {
		return Health{}, err
	}
	if !exists {
		return Health{}, ErrSnapshotUnavailable
	}
	var health Health
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&health); err != nil {
		return Health{}, fmt.Errorf("%w: decode health: %v", ErrSnapshotInvalid, err)
	}
	if health.SchemaVersion != 1 || health.ProducerVersion == "" || health.ProducerInstanceID == "" || health.SessionID == "" || health.ShardID == "" || health.Sequence < 0 || health.ConsecutiveFailures < 0 {
		return Health{}, ErrSnapshotInvalid
	}
	if expectedSession := currentSessionID(worldPath); expectedSession != "" && health.SessionID != expectedSession {
		return Health{}, fmt.Errorf("%w: health session %q does not match %q", ErrSnapshotStale, health.SessionID, expectedSession)
	}
	health.ReadAt = time.Now().UTC()
	if info, statErr := os.Stat(filepath.Join(worldPath, "save", "mod_config_data", "dst-admin", "health.json")); statErr == nil {
		health.ReadAt = info.ModTime().UTC()
	}
	return health, nil
}

func readSnapshot(path, expectedSession string, now time.Time) (Snapshot, error) {
	data, _, exists, err := readRegular(path, maxSnapshotBytes)
	if err != nil {
		return Snapshot{}, err
	}
	if !exists {
		return Snapshot{}, os.ErrNotExist
	}
	var value Snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode JSON: %v", ErrSnapshotInvalid, err)
	}
	if decoder.More() {
		return Snapshot{}, fmt.Errorf("%w: trailing JSON content", ErrSnapshotInvalid)
	}
	if value.SchemaVersion != ProtocolVersion || value.ProducerVersion == "" || value.ProducerInstanceID == "" || value.SessionID == "" || value.ShardID == "" || value.Sequence < 1 || value.CapturedAtUnix < 1 || !value.Complete || value.Players == nil {
		return Snapshot{}, ErrSnapshotInvalid
	}
	value.CapturedAt = time.Unix(value.CapturedAtUnix, 0).UTC()
	if value.CapturedAt.After(now.Add(maxFutureSkew)) {
		return Snapshot{}, fmt.Errorf("%w: capture time is in the future", ErrSnapshotInvalid)
	}
	if now.Sub(value.CapturedAt) > defaultFreshFor {
		return Snapshot{}, fmt.Errorf("%w: captured at %s", ErrSnapshotStale, value.CapturedAt.Format(time.RFC3339))
	}
	if expectedSession != "" && value.SessionID != expectedSession {
		return Snapshot{}, fmt.Errorf("%w: session %q does not match %q", ErrSnapshotStale, value.SessionID, expectedSession)
	}
	if len(value.Players) > 64 {
		return Snapshot{}, fmt.Errorf("%w: too many players", ErrSnapshotInvalid)
	}
	seen := make(map[string]bool, len(value.Players))
	for index := range value.Players {
		player := &value.Players[index]
		if !snapshotPlayerID.MatchString(player.ID) || seen[player.ID] || strings.TrimSpace(player.Name) == "" || len([]rune(player.Name)) > 256 || len([]rune(player.Prefab)) > 128 || len([]rune(player.NetID)) > 128 || player.Age < 0 {
			return Snapshot{}, fmt.Errorf("%w: invalid player identity", ErrSnapshotInvalid)
		}
		if player.NetScore != nil && *player.NetScore < 0 {
			return Snapshot{}, fmt.Errorf("%w: invalid network score", ErrSnapshotInvalid)
		}
		if !validMetric(player.HealthPercent) || !validMetric(player.HungerPercent) || !validMetric(player.SanityPercent) || !validMetric(player.Temperature) || !validMetric(player.Moisture) {
			return Snapshot{}, fmt.Errorf("%w: invalid player metric", ErrSnapshotInvalid)
		}
		seen[player.ID] = true
	}
	return value, nil
}

func validMetric(value *float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func currentSessionID(worldPath string) string {
	root := filepath.Join(worldPath, "save", "session")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	type session struct {
		name    string
		updated time.Time
	}
	values := make([]session, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || len(entry.Name()) > 128 {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		values = append(values, session{name: entry.Name(), updated: info.ModTime()})
	}
	if len(values) == 0 {
		return ""
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].updated.After(values[j].updated) })
	return values[0].name
}
