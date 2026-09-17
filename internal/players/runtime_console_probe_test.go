package players

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"dont/internal/dstruntime"
	"dont/shared"
	lua "github.com/yuin/gopher-lua"
)

type remotePlayerLocality struct{}

func (remotePlayerLocality) IsLocalPlacement(string, string) (bool, error) { return false, nil }

type runtimePlayerConsoleFixture struct {
	t            *testing.T
	output       string
	sent         int
	empty        bool
	noCompletion bool
	err          error
}

func (f *runtimePlayerConsoleFixture) ReadLogs(ctx context.Context, roomID, worldID string, request shared.RuntimeLogRequest) (shared.RuntimeLogChunk, error) {
	if roomID != "remote-room" || worldID != "caves" || !request.Raw {
		f.t.Fatal("wrong remote log request")
	}
	if f.err != nil {
		return shared.RuntimeLogChunk{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return shared.RuntimeLogChunk{}, err
	}
	if request.Cursor < 0 {
		return shared.RuntimeLogChunk{FileID: "remote-log", Size: 100, Cursor: 100}, nil
	}
	start := int(request.Cursor) - 100
	end := min(start+31, len(f.output))
	return shared.RuntimeLogChunk{FileID: "remote-log", Cursor: int64(100 + end), Size: int64(100 + len(f.output)), Data: []byte(f.output[start:end])}, nil
}

func (f *runtimePlayerConsoleFixture) SendID(_ context.Context, roomID, worldID string, request shared.RuntimeConsoleRequest) (shared.RuntimeOperationResult, error) {
	if roomID != "remote-room" || worldID != "caves" || request.Mode != shared.ConsoleModeProbe || request.CoalesceKey == "" {
		f.t.Fatal("wrong remote console request")
	}
	f.sent++
	nonce := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f-]{27}`).FindString(request.Command)
	if nonce == "" {
		f.t.Fatal("missing probe nonce")
	}
	if !f.empty {
		f.output = fmt.Sprintf("[DST-ADMIN-PLAYERS %s ITEM] KU_REMOTE\tPlayer\twendy\t3\t0\t123\t40\t90\t80\t70\t20\t0\talive\n", nonce)
	}
	if !f.noCompletion {
		f.output += fmt.Sprintf("[DST-ADMIN-PLAYERS %s DONE]\n", nonce)
	}
	return shared.RuntimeOperationResult{}, nil
}

func TestRuntimeConsoleProbeReadsRemoteChunksAndRequiresCompletion(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			runtime := &runtimePlayerConsoleFixture{t: t, empty: empty}
			probe := NewRuntimeConsoleProbe(runtime)
			values, err := probe.Snapshot(context.Background(), "remote-room", "caves")
			if err != nil || runtime.sent != 1 {
				t.Fatalf("values=%#v err=%v sent=%d", values, err, runtime.sent)
			}
			if empty {
				if len(values) != 0 {
					t.Fatal(values)
				}
				return
			}
			if len(values) != 1 || values[0].ID != "KU_REMOTE" || values[0].Health != nil || values[0].HealthPercent == nil {
				t.Fatalf("invalid observations: %#v", values)
			}
		})
	}
	runtime := &runtimePlayerConsoleFixture{t: t, noCompletion: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := NewRuntimeConsoleProbe(runtime).Snapshot(ctx, "remote-room", "caves"); !errors.Is(err, ErrProbeTimedOut) {
		t.Fatalf("incomplete response accepted: %v", err)
	}
}

func TestRemoteTelemetryFallbackNeverUsesLocalFiles(t *testing.T) {
	native, local := &telemetryTestProbe{}, &telemetryTestProbe{}
	reader := &telemetryTestReader{err: dstruntime.ErrSnapshotStale}
	probe, err := NewTelemetryProbe(native, reader, local, remotePlayerLocality{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimePlayerConsoleFixture{t: t}
	probe.ConfigureRemoteFallback(NewRuntimeConsoleProbe(runtime))
	result, err := probe.SnapshotDetailed(context.Background(), "remote-room", "caves")
	if err != nil || len(result.Observations) != 1 || !result.Degraded || result.Source != SourceConsoleFallback {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	player := result.Observations[0]
	if player.Fields["online"].Status != FreshnessLive || player.Fields["health"].Status != FreshnessUnavailable {
		t.Fatalf("fabricated freshness: %#v", player.Fields)
	}
	if native.calls != 0 || local.calls != 0 {
		t.Fatal("remote probe read local files")
	}
	runtime.err = errors.New("agent disconnected")
	if _, err := probe.SnapshotDetailed(context.Background(), "remote-room", "caves"); err == nil || !strings.Contains(err.Error(), "agent disconnected") {
		t.Fatalf("remote failure hidden: %v", err)
	}
	reader.err = errors.New("runtime transport disconnected")
	sent := runtime.sent
	if _, err := probe.SnapshotDetailed(context.Background(), "remote-room", "caves"); err == nil || runtime.sent != sent {
		t.Fatalf("retried disconnected transport: %v", err)
	}
}

func TestMaintenanceRosterScriptFitsConsoleAndCountsPlayersWhilePaused(t *testing.T) {
	const nonce = "00000000-0000-0000-0000-000000000000"
	script := playerRosterScript(nonce)
	if len(script) > shared.MaximumRuntimeConsoleCommandBytes {
		t.Fatalf("roster exceeds console limit: %d bytes", len(script))
	}
	state := lua.NewState()
	defer state.Close()
	var output strings.Builder
	state.SetGlobal("print", state.NewFunction(func(l *lua.LState) int {
		output.WriteString(l.CheckString(1) + "\n")
		return 0
	}))
	if err := state.DoString(`TheNet={GetClientTable=function() return {
		{userid="KU_ONE",name="[Host]",admin=true},
		{userid="KU_ONE",name="玩家 一",netid="123",prefab="wendy",playerage=12},
		{userid="KU_TWO",name="Selecting",netid="456"}
	} end}; IsPaused=function() return true end`); err != nil {
		t.Fatal(err)
	}
	if err := state.DoString(script); err != nil {
		t.Fatal(err)
	}
	values, complete, err := parseProbeOutput(output.String(), nonce)
	if err != nil || !complete || len(values) != 2 || values[0].Name != "玩家 一" || values[0].HealthPercent != nil {
		t.Fatalf("invalid paused roster: values=%+v complete=%v err=%v", values, complete, err)
	}
}
