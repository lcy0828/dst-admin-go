package players

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dont/internal/rooms"

	"github.com/google/uuid"
)

const (
	probeTimeout = 6 * time.Second
	probeLimit   = int64(2 * 1024 * 1024)
)

type Probe interface {
	Snapshot(context.Context, string, string) ([]Observation, error)
}

type ProbeSender interface {
	Send(context.Context, string, string, string) error
}

type ProbeCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type LogProbe struct {
	saveRoot string
	rooms    ProbeCatalog
	sender   ProbeSender
}

func NewLogProbe(saveRoot string, roomCatalog ProbeCatalog, sender ProbeSender) (*LogProbe, error) {
	if roomCatalog == nil || sender == nil {
		return nil, errors.New("room catalog and probe sender are required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root is required")
	}
	return &LogProbe{saveRoot: root, rooms: roomCatalog, sender: sender}, nil
}

func (p *LogProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
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
	offset, err := probeLogSize(logPath)
	if err != nil {
		return nil, err
	}
	nonce := uuid.NewString()
	if err := p.sender.Send(ctx, room.DirectoryName, world.DirectoryName, playerProbeScript(nonce)); err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		observations, complete, readErr := readProbeResult(logPath, offset, nonce)
		if readErr != nil {
			return nil, readErr
		}
		if complete {
			return observations, nil
		}
		select {
		case <-probeCtx.Done():
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrProbeTimedOut
			}
			return nil, probeCtx.Err()
		case <-ticker.C:
		}
	}
}

func playerProbeScript(nonce string) string {
	return `local __p="[DST-".."ADMIN-PLAYERS ` + nonce + `"; ` +
		`local function __e(v) return (tostring(v or ""):gsub("([^%w%-%._])",function(c) return string.format("%%%02X",string.byte(c)) end)) end; ` +
		`for _,v in ipairs(TheNet:GetClientTable() or {}) do local p=UserToPlayer(v.userid); ` +
		`local h=-1 local u=-1 local s=-1 local t=-999 local m=-1; ` +
		`if p then if p.components.health then h=p.components.health:GetPercent()*100 end; ` +
		`if p.components.hunger then u=p.components.hunger:GetPercent()*100 end; ` +
		`if p.components.sanity then s=p.components.sanity:GetPercent()*100 end; ` +
		`if p.components.temperature then t=p.components.temperature.current end; ` +
		`if p.components.moisture then m=p.components.moisture:GetMoisture() end end; ` +
		`print(__p.." ITEM] "..table.concat({__e(v.userid),__e(v.name),__e(v.prefab),tostring(v.playerage or 0),v.admin and "1" or "0",__e(v.netid),tostring(v.performance or 0),tostring(h),tostring(u),tostring(s),tostring(t),tostring(m)},"\t")) end; ` +
		`print(__p.." DONE]")`
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

func probeLogSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("player probe log is unsafe")
	}
	return info.Size(), nil
}

func readProbeResult(path string, offset int64, nonce string) ([]Observation, bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return []Observation{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errors.New("player probe log is unsafe")
	}
	if info.Size() < offset {
		offset = 0
	}
	length := info.Size() - offset
	if length > probeLimit {
		return nil, false, errors.New("player probe output exceeds 2 MiB")
	}
	if length == 0 {
		return []Observation{}, false, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, probeLimit+1))
	if err != nil {
		return nil, false, err
	}
	return parseProbeOutput(string(data), nonce)
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
		fields := strings.Split(strings.TrimSpace(line[index+len(itemMarker):]), "\t")
		if len(fields) != 12 {
			return nil, false, errors.New("player probe returned malformed fields")
		}
		decoded := make([]string, 6)
		for fieldIndex := 0; fieldIndex < 6; fieldIndex++ {
			value, decodeErr := url.PathUnescape(fields[fieldIndex])
			if decodeErr != nil {
				return nil, false, errors.New("player probe returned invalid escaping")
			}
			decoded[fieldIndex] = value
		}
		if !ValidID(decoded[0]) || seen[decoded[0]] || len([]rune(decoded[1])) > 256 || len([]rune(decoded[2])) > 128 {
			return nil, false, errors.New("player probe returned invalid identity data")
		}
		seen[decoded[0]] = true
		age, parseErr := strconv.Atoi(fields[3])
		if parseErr != nil || age < 0 {
			return nil, false, errors.New("player probe returned invalid age")
		}
		performance, parseErr := strconv.Atoi(fields[6])
		if parseErr != nil {
			return nil, false, errors.New("player probe returned invalid performance")
		}
		observation := Observation{
			ID: decoded[0], Name: decoded[1], Prefab: decoded[2], Age: age, Admin: fields[4] == "1",
			NetID: decoded[5], Performance: performance,
		}
		metrics := []*(*float64){&observation.HealthPercent, &observation.HungerPercent, &observation.SanityPercent, &observation.Temperature, &observation.Moisture}
		for metricIndex, destination := range metrics {
			value, valueErr := strconv.ParseFloat(fields[7+metricIndex], 64)
			if valueErr != nil {
				return nil, false, errors.New("player probe returned invalid metrics")
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
		observations = append(observations, observation)
		if len(observations) > 64 {
			return nil, false, errors.New("player probe exceeds room player limit")
		}
	}
	return observations, complete, nil
}

type MemoryProbe struct{}

func (MemoryProbe) Snapshot(_ context.Context, _ string, worldID string) ([]Observation, error) {
	value := func(number float64) *float64 { return &number }
	if decoded, _ := rooms.DecodeID(worldID); strings.EqualFold(decoded, "Master") {
		return []Observation{{
			ID: "KU_E2E_ONE", Name: "Willow", Prefab: "willow", Admin: true, Age: 42, NetID: "76561198000000001", Performance: 5,
			HealthPercent: value(92), HungerPercent: value(61), SanityPercent: value(74), Temperature: value(31), Moisture: value(8),
		}}, nil
	}
	return []Observation{{
		ID: "KU_E2E_TWO", Name: "Wilson", Prefab: "wilson", Age: 18, NetID: "76561198000000002", Performance: 4,
		HealthPercent: value(80), HungerPercent: value(55), SanityPercent: value(88), Temperature: value(22), Moisture: value(0),
	}}, nil
}

var _ Probe = (*LogProbe)(nil)
var _ Probe = MemoryProbe{}
