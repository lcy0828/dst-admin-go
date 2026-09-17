package players

import (
	"context"
	"errors"
	"strings"
	"time"

	"dont/shared"

	"github.com/google/uuid"
)

type RuntimeConsoleReader interface {
	ReadLogs(context.Context, string, string, shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error)
	SendID(context.Context, string, string, shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error)
}

type RuntimeConsoleProbe struct {
	runtime    RuntimeConsoleReader
	rosterOnly bool
}

func NewRuntimeConsoleProbe(runtime RuntimeConsoleReader) *RuntimeConsoleProbe {
	return &RuntimeConsoleProbe{runtime: runtime}
}

func NewRuntimeRosterProbe(runtime RuntimeConsoleReader) *RuntimeConsoleProbe {
	return &RuntimeConsoleProbe{runtime: runtime, rosterOnly: true}
}

func (p *RuntimeConsoleProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	probeCtx, cancel := context.WithTimeout(ctx, consoleProbeTimeout)
	defer cancel()
	request := shared.RuntimeLogRequest{Source: shared.RuntimeLogSourceServer, Cursor: -1, MaxBytes: 1, Raw: true}
	initial, err := p.runtime.ReadLogs(probeCtx, roomID, worldID, request)
	if err != nil {
		return nil, err
	}
	request.FileID, request.Cursor, request.MaxBytes = initial.FileID, initial.Size, 64*1024
	nonce := uuid.NewString()
	command := playerProbeScript(nonce)
	if p.rosterOnly {
		command = playerRosterScript(nonce)
	}
	if _, err := p.runtime.SendID(probeCtx, roomID, worldID, shared.RuntimeConsoleRequest{
		// Different nonces require different replies. Coalescing just the send
		// would leave another reader waiting for output the game never emits.
		Mode: shared.ConsoleModeProbe, CoalesceKey: "players-" + nonce, Command: command,
	}); err != nil {
		return nil, err
	}
	var output strings.Builder
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		chunk, err := p.runtime.ReadLogs(probeCtx, roomID, worldID, request)
		if err != nil {
			return nil, err
		}
		if chunk.Reset {
			output.Reset()
		}
		if int64(output.Len()+len(chunk.Data)) > consoleProbeLimit {
			return nil, errors.New("player probe output exceeds 2 MiB")
		}
		output.Write(chunk.Data)
		request.FileID, request.Cursor = chunk.FileID, chunk.Cursor
		// A chunk may end in the middle of an ITEM line. Parse complete lines only.
		if end := strings.LastIndexByte(output.String(), '\n'); end >= 0 {
			values, complete, err := parseProbeOutput(output.String()[:end+1], nonce)
			if err != nil || complete {
				return values, err
			}
		}
		if chunk.Cursor < chunk.Size && len(chunk.Data) > 0 {
			continue
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

// The roster uses the existing parser and deliberately leaves vitals unknown.
// No game component sampling is needed to decide whether maintenance can start.
func playerRosterScript(nonce string) string {
	return `local q="[DST-".."ADMIN-PLAYERS ` + nonce + `"; ` +
		`local function e(v) return (tostring(v or ""):gsub("([^%w%-%._])",function(c) return string.format("%%%02X",string.byte(c)) end)) end; ` +
		`for _,v in ipairs(TheNet:GetClientTable() or {}) do print(q.." ITEM] "..table.concat({e(v.userid),e(v.name),e(v.prefab),tostring(v.playerage or 0),v.admin and "1" or "0",e(v.netid),"-1","-1","-1","-1","-999","-1"},"\t")) end; print(q.." DONE]")`
}
