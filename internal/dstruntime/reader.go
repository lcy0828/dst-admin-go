package dstruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"dont/internal/rooms"

	"github.com/go-ini/ini"
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

func (m *Manager) ReadWorldState(ctx context.Context, roomID, worldID string) (WorldStateSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return WorldStateSnapshot{}, err
	}
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	if !room.Managed {
		return WorldStateSnapshot{}, rooms.ErrRoomNotManaged
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	status := m.inspectAt(room, world, worldPath)
	if status.State == InstallStateMissing {
		return WorldStateSnapshot{}, ErrRuntimeNotInstalled
	}
	if status.State != InstallStateInstalled {
		return WorldStateSnapshot{}, fmt.Errorf("%w: %s", ErrRuntimeNotInstalled, status.Message)
	}
	expectedShard, err := configuredShardID(worldPath)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	expectedSession := currentSessionID(worldPath)
	candidates := make([]WorldStateSnapshot, 0, 2)
	var failures error
	for _, name := range []string{"worldstate-a.json", "worldstate-b.json"} {
		value, readErr := readWorldStateSnapshot(
			filepath.Join(worldPath, "save", "mod_config_data", "dst-admin", name),
			expectedSession,
			expectedShard,
			m.now().UTC(),
		)
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
			return WorldStateSnapshot{}, fmt.Errorf("%w: %v", ErrSnapshotUnavailable, failures)
		}
		return WorldStateSnapshot{}, ErrSnapshotUnavailable
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
	if err := decodeStrictJSON(data, &health); err != nil {
		return Health{}, fmt.Errorf("%w: decode health: %v", ErrSnapshotInvalid, err)
	}
	if health.SchemaVersion != 1 || health.ProducerVersion == "" || health.ProducerInstanceID == "" || health.SessionID == "" || health.ShardID == "" || health.Sequence < 0 || health.ConsecutiveFailures < 0 {
		return Health{}, ErrSnapshotInvalid
	}
	for _, module := range health.Modules {
		if module.Sequence < 0 || module.ConsecutiveFailures < 0 || !validMetric(module.LastDurationMilliseconds) || module.LastCapturedAtUnix != nil && *module.LastCapturedAtUnix < 1 || module.LastWrittenAtUnix != nil && *module.LastWrittenAtUnix < 1 {
			return Health{}, ErrSnapshotInvalid
		}
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
	if err := decodeStrictJSON(data, &value); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode JSON: %v", ErrSnapshotInvalid, err)
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

func readWorldStateSnapshot(path, expectedSession, expectedShard string, now time.Time) (WorldStateSnapshot, error) {
	data, _, exists, err := readRegular(path, maxSnapshotBytes)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	if !exists {
		return WorldStateSnapshot{}, os.ErrNotExist
	}
	var value WorldStateSnapshot
	if err := decodeStrictJSON(data, &value); err != nil {
		return WorldStateSnapshot{}, fmt.Errorf("%w: decode JSON: %v", ErrSnapshotInvalid, err)
	}
	if value.SchemaVersion != ProtocolVersion || value.ProducerVersion != RuntimeVersion || value.ProducerInstanceID == "" || value.SessionID == "" || value.ShardID == "" || value.Sequence < 1 || value.CapturedAtUnix < 1 || !value.Complete {
		return WorldStateSnapshot{}, ErrSnapshotInvalid
	}
	for _, text := range []struct {
		value string
		limit int
	}{
		{value.ProducerVersion, 64}, {value.ProducerInstanceID, 128}, {value.SessionID, 128}, {value.ShardID, 64},
		{value.Season, 64}, {value.Phase, 64}, {value.Precipitation, 64}, {value.MoonPhase, 64}, {value.NightmarePhase, 64},
	} {
		if len([]rune(text.value)) > text.limit || strings.ContainsRune(text.value, '\x00') {
			return WorldStateSnapshot{}, fmt.Errorf("%w: invalid text field", ErrSnapshotInvalid)
		}
	}
	for _, metric := range []*int{value.Cycles, value.ElapsedDaysInSeason, value.RemainingDaysInSeason} {
		if metric != nil && *metric < 0 {
			return WorldStateSnapshot{}, fmt.Errorf("%w: invalid world counter", ErrSnapshotInvalid)
		}
	}
	for _, metric := range []*float64{
		value.SeasonProgress, value.DayProgress, value.PhaseProgress, value.Temperature, value.Wetness,
		value.Moisture, value.MoistureCeil, value.PrecipitationRate, value.NightmareProgress,
	} {
		if !validMetric(metric) {
			return WorldStateSnapshot{}, fmt.Errorf("%w: invalid world metric", ErrSnapshotInvalid)
		}
	}
	value.CapturedAt = time.Unix(value.CapturedAtUnix, 0).UTC()
	if value.CapturedAt.After(now.Add(maxFutureSkew)) {
		return WorldStateSnapshot{}, fmt.Errorf("%w: capture time is in the future", ErrSnapshotInvalid)
	}
	if now.Sub(value.CapturedAt) > defaultFreshFor {
		return WorldStateSnapshot{}, fmt.Errorf("%w: captured at %s", ErrSnapshotStale, value.CapturedAt.Format(time.RFC3339))
	}
	if expectedSession != "" && value.SessionID != expectedSession {
		return WorldStateSnapshot{}, fmt.Errorf("%w: session %q does not match %q", ErrSnapshotStale, value.SessionID, expectedSession)
	}
	if expectedShard != "" && value.ShardID != expectedShard {
		return WorldStateSnapshot{}, fmt.Errorf("%w: shard %q does not match %q", ErrSnapshotStale, value.ShardID, expectedShard)
	}
	return value, nil
}

func decodeStrictJSON(data []byte, destination interface{}) error {
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

func configuredShardID(worldPath string) (string, error) {
	clusterData, _, clusterExists, err := readRegular(filepath.Join(filepath.Dir(worldPath), "cluster.ini"), 256*1024)
	if err != nil {
		return "", err
	}
	if clusterExists {
		cluster, loadErr := ini.Load(clusterData)
		if loadErr != nil {
			return "", fmt.Errorf("parse cluster.ini shard identity: %w", loadErr)
		}
		shardEnabled := cluster.Section("SHARD").Key("shard_enabled")
		if shardEnabled.String() != "" {
			enabled, boolErr := shardEnabled.Bool()
			if boolErr != nil {
				return "", fmt.Errorf("parse cluster.ini shard_enabled: %w", boolErr)
			}
			if !enabled {
				return "0", nil
			}
		}
	}
	data, _, exists, err := readRegular(filepath.Join(worldPath, "server.ini"), 256*1024)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	config, err := ini.Load(data)
	if err != nil {
		return "", fmt.Errorf("parse server.ini shard identity: %w", err)
	}
	return strings.TrimSpace(config.Section("SHARD").Key("id").String()), nil
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
