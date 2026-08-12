package players

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"dont/internal/rooms"
)

const nativeProbeLineLimit = 1024 * 1024

type Probe interface {
	Snapshot(context.Context, string, string) ([]Observation, error)
}

type HistoryProbe interface {
	HistorySnapshot(context.Context, string, string) ([]Observation, error)
}

type ProbeCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type LogProbe struct {
	saveRoot string
	rooms    ProbeCatalog
	mu       sync.Mutex
	logs     map[string]*nativeLogState
	known    map[string]map[string]Observation
}

type nativeLogState struct {
	identity os.FileInfo
	position int64
	pending  string
	known    map[string]Observation
	online   map[string]bool
	adminSet map[string]bool
	blocks   map[string]*historicalProbeBlock
}

func NewLogProbe(saveRoot string, roomCatalog ProbeCatalog) (*LogProbe, error) {
	if roomCatalog == nil {
		return nil, errors.New("room catalog is required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root is required")
	}
	return &LogProbe{
		saveRoot: root, rooms: roomCatalog, logs: make(map[string]*nativeLogState), known: make(map[string]map[string]Observation),
	}, nil
}

func (p *LogProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	return p.nativeObservations(ctx, roomID, worldID, true)
}

func (p *LogProbe) HistorySnapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	return p.nativeObservations(ctx, roomID, worldID, false)
}

func (p *LogProbe) nativeObservations(ctx context.Context, roomID, worldID string, onlineOnly bool) ([]Observation, error) {
	room, err := p.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	world, err := p.rooms.World(roomID, worldID)
	if err != nil {
		return nil, err
	}
	logPath, err := safeProbeLog(p.saveRoot, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return nil, err
	}
	info, err := probeLogInfo(logPath)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return []Observation{}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.logs[logPath]
	if state == nil || state.identity == nil || !os.SameFile(state.identity, info) || info.Size() < state.position {
		state = newNativeLogState(info)
		p.logs[logPath] = state
	}
	if info.Size() > state.position {
		position, pending, readErr := consumeNativeLog(ctx, logPath, state.position, info.Size(), state.pending, func(line string) {
			p.applyNativeLine(room.ID, state, line)
		})
		if readErr != nil {
			return nil, readErr
		}
		state.position, state.pending, state.identity = position, pending, info
	}
	ids := make([]string, 0, len(state.known))
	if onlineOnly {
		for id := range state.online {
			ids = append(ids, id)
		}
	} else {
		for id := range state.known {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	observations := make([]Observation, 0, len(ids))
	for _, id := range ids {
		observation := state.known[id]
		if roomKnown := p.known[room.ID]; roomKnown != nil {
			observation = mergeObservation(roomKnown[id], observation)
		}
		if state.adminSet[id] {
			observation.Admin = state.known[id].Admin
		}
		if observation.Name == "" {
			observation.Name = observation.ID
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func safeProbeLog(root, roomName, worldName string) (string, error) {
	if filepath.Base(roomName) != roomName || filepath.Base(worldName) != worldName || roomName == "" || worldName == "" {
		return "", errors.New("unsafe room or world directory")
	}
	path := filepath.Join(root, roomName, worldName, "server_log.txt")
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("player probe log escapes save root")
	}
	return path, nil
}

func probeLogInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("player probe log is unsafe")
	}
	return info, nil
}

func newNativeLogState(info os.FileInfo) *nativeLogState {
	return &nativeLogState{
		identity: info, known: make(map[string]Observation), online: make(map[string]bool), adminSet: make(map[string]bool), blocks: make(map[string]*historicalProbeBlock),
	}
}

func consumeNativeLog(ctx context.Context, path string, position, size int64, pending string, consume func(string)) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return position, pending, err
	}
	defer file.Close()
	if _, err := file.Seek(position, io.SeekStart); err != nil {
		return position, pending, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, size-position), 64*1024)
	cursor := position
	for {
		if err := ctx.Err(); err != nil {
			return position, pending, err
		}
		part, readErr := reader.ReadString('\n')
		cursor += int64(len(part))
		if len(pending)+len(part) > nativeProbeLineLimit {
			return position, pending, errors.New("player source log line exceeds 1 MiB")
		}
		part = pending + part
		pending = ""
		if strings.HasSuffix(part, "\n") {
			consume(strings.TrimSuffix(strings.TrimSuffix(part, "\n"), "\r"))
		} else {
			pending = part
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return position, pending, readErr
		}
	}
	return cursor, pending, nil
}

func (p *LogProbe) applyNativeLine(roomID string, state *nativeLogState, line string) {
	if observation, authenticated := parseAuthenticatedClient(line); authenticated {
		state.known[observation.ID] = mergeObservation(state.known[observation.ID], observation)
		state.online[observation.ID] = true
		p.rememberObservation(roomID, observation)
		return
	}
	if observation, initialized := parseInitializedClient(line); initialized {
		current := mergeObservation(state.known[observation.ID], observation)
		current.Admin = observation.Admin
		state.known[observation.ID] = current
		state.online[observation.ID] = true
		state.adminSet[observation.ID] = true
		p.rememberInitializedObservation(roomID, observation)
		return
	}
	if id, prefab, owned := parsePlayerOwnership(line); owned {
		observation := state.known[id]
		observation.ID = id
		observation.Prefab = prefab
		state.known[id] = observation
		state.online[id] = true
		p.rememberObservation(roomID, observation)
		return
	}
	if id, disconnected := parseDisconnectedClient(line); disconnected {
		delete(state.online, id)
		return
	}
	applyHistoricalProbeLine(state, line, func(observations []Observation) {
		for _, observation := range observations {
			state.known[observation.ID] = mergeObservation(state.known[observation.ID], observation)
			p.rememberObservation(roomID, observation)
		}
	})
}

func (p *LogProbe) rememberObservation(roomID string, observation Observation) {
	if observation.ID == "" {
		return
	}
	if p.known[roomID] == nil {
		p.known[roomID] = make(map[string]Observation)
	}
	p.known[roomID][observation.ID] = mergeObservation(p.known[roomID][observation.ID], observation)
}

func (p *LogProbe) rememberInitializedObservation(roomID string, observation Observation) {
	p.rememberObservation(roomID, observation)
	known := p.known[roomID][observation.ID]
	known.Admin = observation.Admin
	p.known[roomID][observation.ID] = known
}

func parseProbeOutput(output, nonce string) ([]Observation, bool, error) {
	itemMarker := "[DST-ADMIN-PLAYERS " + nonce + " ITEM] "
	doneMarker := "[DST-ADMIN-PLAYERS " + nonce + " DONE]"
	observations := make([]Observation, 0)
	seen := make(map[string]bool)
	complete := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, doneMarker) {
			complete = true
		}
		index := strings.Index(line, itemMarker)
		if index < 0 {
			continue
		}
		observation, ignored, err := parseProbeObservation(line[index+len(itemMarker):], true)
		if err != nil {
			return nil, false, err
		}
		if ignored {
			continue
		}
		if seen[observation.ID] {
			return nil, false, errors.New("player probe returned invalid identity data")
		}
		seen[observation.ID] = true
		observations = append(observations, observation)
		if len(observations) > 64 {
			return nil, false, errors.New("player probe exceeds room player limit")
		}
	}
	return observations, complete, nil
}

type historicalProbeBlock struct {
	observations []Observation
	seen         map[string]bool
	invalid      bool
}

func parseProbeHistory(output string) []Observation {
	state := newNativeLogState(nil)
	var latest []Observation
	for _, line := range strings.Split(output, "\n") {
		applyHistoricalProbeLine(state, line, func(observations []Observation) {
			latest = append([]Observation(nil), observations...)
		})
	}
	return latest
}

func applyHistoricalProbeLine(state *nativeLogState, line string, complete func([]Observation)) {
	const marker = "[DST-ADMIN-PLAYERS "
	index := strings.Index(line, marker)
	if index < 0 {
		return
	}
	remainder := line[index+len(marker):]
	if itemEnd := strings.Index(remainder, " ITEM] "); itemEnd > 0 {
		nonce := remainder[:itemEnd]
		block := state.blocks[nonce]
		if block == nil {
			block = &historicalProbeBlock{seen: make(map[string]bool)}
			state.blocks[nonce] = block
		}
		observation, ignored, err := parseProbeObservation(remainder[itemEnd+len(" ITEM] "):], false)
		if err != nil {
			block.invalid = true
			return
		}
		if ignored || block.seen[observation.ID] {
			return
		}
		block.seen[observation.ID] = true
		block.observations = append(block.observations, observation)
		if len(block.observations) > 64 {
			block.invalid = true
		}
		return
	}
	if doneEnd := strings.Index(remainder, " DONE]"); doneEnd > 0 {
		nonce := remainder[:doneEnd]
		if block := state.blocks[nonce]; block != nil && !block.invalid && len(block.observations) > 0 {
			complete(block.observations)
		}
		delete(state.blocks, nonce)
	}
}

func parseAuthenticatedClient(line string) (Observation, bool) {
	const marker = "Client authenticated: ("
	start := strings.Index(line, marker)
	if start < 0 {
		return Observation{}, false
	}
	payload := line[start+len(marker):]
	end := strings.Index(payload, ") ")
	if end < 0 {
		return Observation{}, false
	}
	id := strings.TrimSpace(payload[:end])
	name := strings.TrimSpace(payload[end+2:])
	if !ValidID(id) || name == "" || len([]rune(name)) > 256 {
		return Observation{}, false
	}
	return Observation{ID: id, Name: name}, true
}

func parseInitializedClient(line string) (Observation, bool) {
	if !strings.Contains(line, "[ClientObject] Initialized (authenticated)") {
		return Observation{}, false
	}
	fields := strings.Fields(line)
	observation := Observation{}
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "userid":
			observation.ID = value
		case "netid":
			observation.NetID = value
		case "admin":
			observation.Admin = value == "1" || strings.EqualFold(value, "true")
		}
	}
	if !ValidID(observation.ID) {
		return Observation{}, false
	}
	return observation, true
}

func parsePlayerOwnership(line string) (string, string, bool) {
	const marker = "User ID\t"
	start := strings.Index(line, marker)
	if start < 0 || !strings.Contains(line, "\tassigned ownership to entity\t") {
		return "", "", false
	}
	payload := line[start+len(marker):]
	idEnd := strings.IndexByte(payload, '\t')
	entity := strings.LastIndex(payload, " - ")
	if idEnd < 0 || entity < 0 {
		return "", "", false
	}
	id := strings.TrimSpace(payload[:idEnd])
	prefab := strings.TrimSpace(payload[entity+3:])
	if !ValidID(id) || prefab == "" || len([]rune(prefab)) > 128 {
		return "", "", false
	}
	return id, prefab, true
}

func parseDisconnectedClient(line string) (string, bool) {
	const marker = "[Shard] ("
	start := strings.Index(line, marker)
	if start < 0 {
		return "", false
	}
	payload := line[start+len(marker):]
	end := strings.Index(payload, ") disconnected from ")
	if end < 0 {
		return "", false
	}
	id := strings.TrimSpace(payload[:end])
	return id, ValidID(id)
}

func mergeObservation(existing, update Observation) Observation {
	if existing.ID == "" {
		existing.ID = update.ID
	}
	if update.Name != "" {
		existing.Name = update.Name
	}
	if update.Prefab != "" {
		existing.Prefab = update.Prefab
	}
	if update.NetID != "" {
		existing.NetID = update.NetID
	}
	if update.Admin {
		existing.Admin = true
	}
	if update.Age > 0 {
		existing.Age = update.Age
	}
	if update.NetScore != nil {
		existing.NetScore = update.NetScore
	}
	if update.HealthPercent != nil {
		existing.HealthPercent = update.HealthPercent
	}
	if update.HungerPercent != nil {
		existing.HungerPercent = update.HungerPercent
	}
	if update.SanityPercent != nil {
		existing.SanityPercent = update.SanityPercent
	}
	if update.Temperature != nil {
		existing.Temperature = update.Temperature
	}
	if update.Moisture != nil {
		existing.Moisture = update.Moisture
	}
	if existing.Fields == nil && len(update.Fields) > 0 {
		existing.Fields = make(FieldStates, len(update.Fields))
	}
	for field, state := range update.Fields {
		existing.Fields[field] = state
	}
	return existing
}

func parseProbeObservation(payload string, captureNetScore bool) (Observation, bool, error) {
	fields := strings.Split(strings.TrimSpace(payload), "\t")
	if len(fields) != 12 {
		return Observation{}, false, errors.New("player probe returned malformed fields")
	}
	decoded := make([]string, 6)
	for fieldIndex := 0; fieldIndex < 6; fieldIndex++ {
		value, err := url.PathUnescape(fields[fieldIndex])
		if err != nil {
			return Observation{}, false, errors.New("player probe returned invalid escaping")
		}
		decoded[fieldIndex] = value
	}
	if virtualHostObservation(decoded) {
		return Observation{}, true, nil
	}
	if !ValidID(decoded[0]) || len([]rune(decoded[1])) > 256 || len([]rune(decoded[2])) > 128 {
		return Observation{}, false, errors.New("player probe returned invalid identity data")
	}
	age, err := strconv.Atoi(fields[3])
	if err != nil || age < 0 {
		return Observation{}, false, errors.New("player probe returned invalid age")
	}
	netScore, err := strconv.Atoi(fields[6])
	if err != nil {
		return Observation{}, false, errors.New("player probe returned invalid network score")
	}
	observation := Observation{
		ID: decoded[0], Name: decoded[1], Prefab: decoded[2], Age: age, Admin: fields[4] == "1", NetID: decoded[5],
	}
	if captureNetScore && netScore >= 0 {
		observation.NetScore = &netScore
	}
	metrics := []*(*float64){&observation.HealthPercent, &observation.HungerPercent, &observation.SanityPercent, &observation.Temperature, &observation.Moisture}
	for metricIndex, destination := range metrics {
		value, err := strconv.ParseFloat(fields[7+metricIndex], 64)
		if err != nil {
			return Observation{}, false, errors.New("player probe returned invalid metrics")
		}
		unavailable := value < 0
		if metricIndex == 3 {
			unavailable = value <= -999
		}
		if !unavailable {
			metric := value
			*destination = &metric
		}
	}
	return observation, false, nil
}

func virtualHostObservation(identity []string) bool {
	return len(identity) >= 6 && identity[1] == "[Host]" && identity[2] == "" && identity[5] == ""
}

type MemoryProbe struct{}

func (MemoryProbe) Snapshot(_ context.Context, _ string, worldID string) ([]Observation, error) {
	value := func(number float64) *float64 { return &number }
	if decoded, _ := rooms.DecodeID(worldID); strings.EqualFold(decoded, "Master") {
		netScore := 0
		return []Observation{{
			ID: "KU_E2E_ONE", Name: "Willow", Prefab: "willow", Admin: true, Age: 42, NetID: "76561198000000001", NetScore: &netScore,
			HealthPercent: value(92), HungerPercent: value(61), SanityPercent: value(74), Temperature: value(31), Moisture: value(8),
		}}, nil
	}
	netScore := 1
	return []Observation{{
		ID: "KU_E2E_TWO", Name: "Wilson", Prefab: "wilson", Age: 18, NetID: "76561198000000002", NetScore: &netScore,
		HealthPercent: value(80), HungerPercent: value(55), SanityPercent: value(88), Temperature: value(22), Moisture: value(0),
	}}, nil
}

var _ Probe = (*LogProbe)(nil)
var _ HistoryProbe = (*LogProbe)(nil)
var _ Probe = MemoryProbe{}
