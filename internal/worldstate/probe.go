package worldstate

import (
	"context"
	"errors"
	"io"
	"math"
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
	probeLimit   = int64(256 * 1024)
)

type Sampler interface {
	Snapshot(context.Context, string, string) (Observation, error)
}

type ProbeSender interface {
	Send(context.Context, string, string, string) error
}

type ProbeCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
}

type LogSampler struct {
	saveRoot string
	rooms    ProbeCatalog
	sender   ProbeSender
}

func NewLogSampler(saveRoot string, roomCatalog ProbeCatalog, sender ProbeSender) (*LogSampler, error) {
	if roomCatalog == nil || sender == nil {
		return nil, errors.New("room catalog and probe sender are required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root is required")
	}
	return &LogSampler{saveRoot: root, rooms: roomCatalog, sender: sender}, nil
}

func (p *LogSampler) Snapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	room, err := p.rooms.Room(roomID)
	if err != nil {
		return Observation{}, err
	}
	world, err := p.rooms.World(roomID, worldID)
	if err != nil {
		return Observation{}, err
	}
	logPath, err := safeProbeLog(p.saveRoot, room.DirectoryName, world.DirectoryName)
	if err != nil {
		return Observation{}, err
	}
	offset, err := probeLogSize(logPath)
	if err != nil {
		return Observation{}, err
	}
	nonce := uuid.NewString()
	if err := p.sender.Send(ctx, room.DirectoryName, world.DirectoryName, worldStateProbeScript(nonce)); err != nil {
		return Observation{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		observation, complete, readErr := readProbeResult(logPath, offset, nonce)
		if readErr != nil {
			return Observation{}, readErr
		}
		if complete {
			return observation, nil
		}
		select {
		case <-probeCtx.Done():
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				return Observation{}, ErrProbeTimedOut
			}
			return Observation{}, probeCtx.Err()
		case <-ticker.C:
		}
	}
}

func worldStateProbeScript(nonce string) string {
	return `local __p="[DST-".."ADMIN-WORLDSTATE ` + nonce + `"; ` +
		`local function __e(v) if v==nil then return "" end return (tostring(v):gsub("([^%w%-%._])",function(c) return string.format("%%%02X",string.byte(c)) end)) end; ` +
		`local function __call(o,n) local f=o and o[n]; if type(f)=="function" then local ok,v=pcall(f,o); if ok then return v end end return nil end; ` +
		`local s=(TheWorld and TheWorld.state) or {}; local c=(TheWorld and TheWorld.components) or {}; ` +
		`local sm=c.seasonmanager; local nc=c.nightmareclock; ` +
		`local sp=s.seasonprogress or __call(sm,"GetPercentSeason"); local np=s.nightmaretimeinphase or __call(nc,"GetTimeInPhase"); ` +
		`local nphase=s.nightmarephase or __call(nc,"GetPhase"); local precip="none"; local hp=nil; ` +
		`for _,v in ipairs((TheNet and TheNet:GetClientTable()) or {}) do local p=tonumber(v and v.performance); if p and p>=0 and p<=2 then hp=math.floor(p); break end end; ` +
		`if s.isacidraining then precip="acid_rain" elseif s.islunarhailing then precip="lunar_hail" elseif s.issnowing then precip="snow" elseif s.israining then precip="rain" end; ` +
		`local values={__e(s.season),__e(s.phase),__e(s.cycles),__e(s.elapseddaysinseason),__e(s.remainingdaysinseason),__e(sp),__e(s.time),__e(s.timeinphase),__e(precip),__e(s.moonphase),__e(s.temperature),__e(s.wetness),__e(s.moisture),__e(s.moistureceil),__e(s.precipitationrate),__e(nphase),__e(np),__e(hp)}; ` +
		`print(__p.." ITEM] "..table.concat(values,"\t")); print(__p.." DONE]")`
}

func safeProbeLog(root, roomName, worldName string) (string, error) {
	if filepath.Base(roomName) != roomName || filepath.Base(worldName) != worldName || roomName == "" || worldName == "" {
		return "", errors.New("unsafe room or world directory")
	}
	path := filepath.Join(root, roomName, worldName, "server_log.txt")
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("world state probe log escapes save root")
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
		return 0, errors.New("world state probe log is unsafe")
	}
	return info.Size(), nil
}

func readProbeResult(path string, offset int64, nonce string) (Observation, bool, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Observation{}, false, errors.New("world state probe log is unsafe")
	}
	if info.Size() < offset {
		offset = 0
	}
	length := info.Size() - offset
	if length > probeLimit {
		return Observation{}, false, errors.New("world state probe output exceeds 256 KiB")
	}
	if length == 0 {
		return Observation{}, false, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return Observation{}, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, probeLimit+1))
	if err != nil {
		return Observation{}, false, err
	}
	return parseProbeOutput(string(data), nonce)
}

func parseProbeOutput(output, nonce string) (Observation, bool, error) {
	itemMarker := "[DST-ADMIN-WORLDSTATE " + nonce + " ITEM] "
	doneMarker := "[DST-ADMIN-WORLDSTATE " + nonce + " DONE]"
	var observation Observation
	found := false
	complete := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, doneMarker) {
			complete = true
		}
		index := strings.Index(line, itemMarker)
		if index < 0 {
			continue
		}
		if found {
			return Observation{}, false, errors.New("world state probe returned duplicate snapshots")
		}
		fields := strings.Split(strings.TrimSuffix(line[index+len(itemMarker):], "\r"), "\t")
		if len(fields) != 18 {
			return Observation{}, false, errors.New("world state probe returned malformed fields")
		}
		decoded := make([]string, len(fields))
		for fieldIndex, field := range fields {
			value, decodeErr := url.PathUnescape(field)
			if decodeErr != nil {
				return Observation{}, false, errors.New("world state probe returned invalid escaping")
			}
			decoded[fieldIndex] = value
		}
		if len([]rune(decoded[0])) > 64 || len([]rune(decoded[1])) > 64 || len([]rune(decoded[8])) > 64 || len([]rune(decoded[9])) > 64 || len([]rune(decoded[15])) > 64 {
			return Observation{}, false, errors.New("world state probe returned oversized text")
		}
		var parseErr error
		observation.Season, observation.Phase = decoded[0], decoded[1]
		if observation.Cycles, parseErr = optionalInt(decoded[2]); parseErr != nil {
			return Observation{}, false, parseErr
		}
		if observation.ElapsedDaysInSeason, parseErr = optionalInt(decoded[3]); parseErr != nil {
			return Observation{}, false, parseErr
		}
		if observation.RemainingDaysInSeason, parseErr = optionalInt(decoded[4]); parseErr != nil {
			return Observation{}, false, parseErr
		}
		floatFields := []*(*float64){&observation.SeasonProgress, &observation.DayProgress, &observation.PhaseProgress, &observation.Temperature, &observation.Wetness, &observation.Moisture, &observation.MoistureCeil, &observation.PrecipitationRate, &observation.NightmareProgress}
		indexes := []int{5, 6, 7, 10, 11, 12, 13, 14, 16}
		for metricIndex, destination := range floatFields {
			value, valueErr := optionalFloat(decoded[indexes[metricIndex]])
			if valueErr != nil {
				return Observation{}, false, valueErr
			}
			*destination = value
		}
		observation.Precipitation, observation.MoonPhase = decoded[8], decoded[9]
		observation.NightmarePhase = decoded[15]
		if observation.HostPerformance, parseErr = optionalInt(decoded[17]); parseErr != nil || observation.HostPerformance != nil && *observation.HostPerformance > 2 {
			return Observation{}, false, errors.New("world state probe returned an invalid host performance")
		}
		found = true
	}
	if complete && !found {
		return Observation{}, false, errors.New("world state probe completed without a snapshot")
	}
	return observation, complete && found, nil
}

func optionalInt(value string) (*int, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return nil, errors.New("world state probe returned an invalid integer")
	}
	return &parsed, nil
}

func optionalFloat(value string) (*float64, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return nil, errors.New("world state probe returned an invalid number")
	}
	return &parsed, nil
}

type MemorySampler struct{}

func (MemorySampler) Snapshot(_ context.Context, _ string, worldID string) (Observation, error) {
	integer := func(value int) *int { return &value }
	number := func(value float64) *float64 { return &value }
	worldName, _ := rooms.DecodeID(worldID)
	if strings.Contains(strings.ToLower(worldName), "cave") {
		return Observation{
			Season: "autumn", Phase: "night", Cycles: integer(48), ElapsedDaysInSeason: integer(6), RemainingDaysInSeason: integer(14),
			SeasonProgress: number(.30), DayProgress: number(.76), PhaseProgress: number(.42), Precipitation: "none", MoonPhase: "new",
			Temperature: number(12.5), Wetness: number(.08), Moisture: number(8), MoistureCeil: number(100), PrecipitationRate: number(0),
			NightmarePhase: "warn", NightmareProgress: number(.46),
		}, nil
	}
	return Observation{
		Season: "autumn", Phase: "day", Cycles: integer(48), ElapsedDaysInSeason: integer(6), RemainingDaysInSeason: integer(14),
		SeasonProgress: number(.30), DayProgress: number(.34), PhaseProgress: number(.57), Precipitation: "rain", MoonPhase: "new",
		Temperature: number(18.5), Wetness: number(.18), Moisture: number(18), MoistureCeil: number(100), PrecipitationRate: number(.25),
	}, nil
}

var _ Sampler = (*LogSampler)(nil)
var _ Sampler = MemorySampler{}
