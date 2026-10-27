package players

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	consoleProbeTimeout = 6 * time.Second
	consoleProbeLimit   = int64(2 * 1024 * 1024)
)

type ConsoleProbe struct {
	saveRoot string
	rooms    ProbeCatalog
	sender   Sender
}

func NewConsoleProbe(saveRoot string, roomCatalog ProbeCatalog, sender Sender) (*ConsoleProbe, error) {
	if roomCatalog == nil || sender == nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root, room catalog, and sender are required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil {
		return nil, err
	}
	return &ConsoleProbe{saveRoot: filepath.Clean(root), rooms: roomCatalog, sender: sender}, nil
}

func (p *ConsoleProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
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
	offset, err := consoleProbeLogSize(logPath)
	if err != nil {
		return nil, err
	}
	nonce := uuid.NewString()
	if err := p.sender.Send(ctx, room.DirectoryName, world.DirectoryName, playerProbeScript(nonce)); err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		observations, complete, readErr := readConsoleProbeResult(logPath, offset, nonce)
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

// Keep the fallback small enough for both local and Agent console transports.
// Account metadata is cluster-wide; only AllPlayers proves shard locality.
// Detailed vitals remain unavailable until managed telemetry is healthy.
func playerProbeScript(nonce string) string {
    return `local q="[DST-".."ADMIN-PLAYERS ` + nonce + `";` +
        `local function e(v)return(tostring(v or""):gsub("([^%w%-%._])",function(c)return string.format("%%%02X",c:byte())end))end;` +
        `local a,b={},{};for i,p in ipairs(AllPlayers)do assert(i<257);a[p.userid]=p end;` +
        `for i,v in ipairs(TheNet:GetClientTable()or{})do assert(i<257);if v.userid and(v.name~="[Host]"or(v.prefab or"")~=""or(v.netid or"")~="")then b[v.userid]=v end end;` +
        `for u,p in pairs(a)do b[u]=b[u]or{userid=u,name=p.name,prefab=p.prefab}end;` +
        `for u,v in pairs(b)do local p=a[u];local g=p and(p.migration and"migrating"or p:HasTag("playerghost")and"ghost"or p.components.health and p.components.health:IsDead()and"dead"or"alive")or(v.prefab and v.prefab~=""and"unknown"or"selecting_character");` +
        `print(q.." ITEM] "..table.concat({e(u),e(v.name or u),e(p and p.prefab or v.prefab),v.playerage or 0,v.admin and 1 or 0,e(v.netid),v.netscore or -1,-1,-1,-1,-999,-1,g,p and 1 or 0},"\t"))end;print(q.." DONE]")`
}

func consoleProbeLogSize(path string) (int64, error) {
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

func readConsoleProbeResult(path string, offset int64, nonce string) ([]Observation, bool, error) {
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
	if length > consoleProbeLimit {
		return nil, false, errors.New("player probe output exceeds 2 MiB")
	}
	if length == 0 {
		return []Observation{}, false, nil
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, consoleProbeLimit+1))
	if err != nil {
		return nil, false, err
	}
	return parseProbeOutput(string(data), nonce)
}

var _ Probe = (*ConsoleProbe)(nil)
