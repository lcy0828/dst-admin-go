package players

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/dstruntime"
)

func TestStampNativeObservationsMarksMissingIdentityFieldsUnavailable(t *testing.T) {
	observedAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	values := stampNativeObservations([]Observation{{ID: "KU_ONE", Name: "Willow"}}, observedAt)
	if len(values) != 1 {
		t.Fatalf("observations = %#v", values)
	}
	if values[0].Fields["name"].Status != FreshnessLive {
		t.Fatalf("name state = %#v", values[0].Fields["name"])
	}
	for _, field := range []string{"prefab", "netId"} {
		if state := values[0].Fields[field]; state.Status != FreshnessUnavailable || state.Source != SourceNativeLog {
			t.Fatalf("%s state = %#v", field, state)
		}
	}
}

type telemetryTestProbe struct {
	items []Observation
	err   error
	calls int
}

func (p *telemetryTestProbe) Snapshot(context.Context, string, string) ([]Observation, error) {
	p.calls++
	return append([]Observation(nil), p.items...), p.err
}

type telemetryTestReader struct {
	snapshot dstruntime.Snapshot
	err      error
	calls    int
}

func (r *telemetryTestReader) ReadPlayers(context.Context, string, string) (dstruntime.Snapshot, error) {
	r.calls++
	return r.snapshot, r.err
}

type refreshableTelemetryTestReader struct {
	*telemetryTestReader
	refresh      dstruntime.SnapshotRefreshResult
	refreshErr   error
	refreshCalls int
}

func (r *refreshableTelemetryTestReader) RefreshSnapshots(context.Context, string, string) (dstruntime.SnapshotRefreshResult, error) {
	r.refreshCalls++
	return r.refresh, r.refreshErr
}

func TestTelemetryProbeUsesRuntimeWithoutCallingFallback(t *testing.T) {
	capturedAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	native := &telemetryTestProbe{items: []Observation{
		{ID: "KU_ONE", Name: "Native name", Prefab: "wendy", NetID: "7656119", Admin: true, Age: 99},
		{ID: "KU_STALE", Name: "Stale log player"},
	}}
	runtime := &telemetryTestReader{snapshot: dstruntime.Snapshot{
		CapturedAt: capturedAt,
		Players:    []dstruntime.SnapshotPlayer{{ID: "KU_ONE", Name: "Runtime name", Admin: false, Age: 0}},
	}}
	fallback := &telemetryTestProbe{err: errors.New("fallback must not be called")}
	probe, err := NewTelemetryProbe(native, runtime, fallback)
	if err != nil {
		t.Fatal(err)
	}

	result, err := probe.SnapshotDetailed(context.Background(), "room", "master")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.calls != 0 || result.Source != SourceRuntime || result.Degraded || !result.ObservedAt.Equal(capturedAt) {
		t.Fatalf("unexpected runtime result: %#v fallback calls=%d", result, fallback.calls)
	}
	if len(result.Observations) != 1 {
		t.Fatalf("native-only player entered authoritative snapshot: %#v", result.Observations)
	}
	player := result.Observations[0]
	if player.Name != "Runtime name" || player.Prefab != "wendy" || player.NetID != "7656119" || player.Admin || player.Age != 0 {
		t.Fatalf("identity merge lost authoritative runtime values: %#v", player)
	}
}

func TestTelemetryProbeFallsBackOnlyWhenRuntimeFails(t *testing.T) {
	native := &telemetryTestProbe{items: []Observation{{ID: "KU_ONE", Name: "Native", Prefab: "willow"}}}
	runtime := &telemetryTestReader{err: dstruntime.ErrSnapshotStale}
	fallback := &telemetryTestProbe{items: []Observation{{ID: "KU_ONE", Name: "Fallback"}}}
	probe, err := NewTelemetryProbe(native, runtime, fallback)
	if err != nil {
		t.Fatal(err)
	}
	probe.now = func() time.Time { return time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC) }

	result, err := probe.SnapshotDetailed(context.Background(), "room", "master")
	if err != nil {
		t.Fatal(err)
	}
	if fallback.calls != 1 || result.Source != SourceConsoleFallback || !result.Degraded || !strings.Contains(result.Warning, "fallback") {
		t.Fatalf("unexpected fallback result: %#v fallback calls=%d", result, fallback.calls)
	}
	if len(result.Observations) != 1 || result.Observations[0].Name != "Fallback" || result.Observations[0].Prefab != "willow" {
		t.Fatalf("fallback did not retain native identity: %#v", result.Observations)
	}
}

func TestTelemetryProbeActivelyRefreshesStaleRuntimeBeforeFallback(t *testing.T) {
	capturedAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	native := &telemetryTestProbe{items: []Observation{{ID: "KU_ONE", Prefab: "willow"}}}
	runtime := &refreshableTelemetryTestReader{
		telemetryTestReader: &telemetryTestReader{err: dstruntime.ErrSnapshotStale},
		refresh: dstruntime.SnapshotRefreshResult{Players: dstruntime.Snapshot{
			CapturedAt: capturedAt, Players: []dstruntime.SnapshotPlayer{{ID: "KU_ONE", Name: "Willow"}},
		}},
	}
	fallback := &telemetryTestProbe{err: errors.New("fallback must not be called")}
	probe, err := NewTelemetryProbe(native, runtime, fallback)
	if err != nil {
		t.Fatal(err)
	}
	result, err := probe.SnapshotDetailed(context.Background(), "room", "master")
	if err != nil || runtime.refreshCalls != 1 || fallback.calls != 0 || result.Source != SourceRuntime || result.Degraded {
		t.Fatalf("result = %#v, error = %v, refresh calls = %d, fallback calls = %d", result, err, runtime.refreshCalls, fallback.calls)
	}
}

func TestTelemetryProbeReturnsErrorWhenRuntimeAndFallbackFail(t *testing.T) {
	native := &telemetryTestProbe{err: errors.New("native unavailable")}
	runtime := &telemetryTestReader{err: dstruntime.ErrSnapshotUnavailable}
	fallback := &telemetryTestProbe{err: ErrProbeTimedOut}
	probe, err := NewTelemetryProbe(native, runtime, fallback)
	if err != nil {
		t.Fatal(err)
	}

	result, err := probe.SnapshotDetailed(context.Background(), "room", "master")
	if err == nil || !errors.Is(err, ErrProbeTimedOut) || len(result.Observations) != 0 {
		t.Fatalf("dual failure was accepted: result=%#v err=%v", result, err)
	}
}
