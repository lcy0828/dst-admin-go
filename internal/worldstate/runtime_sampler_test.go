package worldstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"dont/internal/dstruntime"
)

type runtimeWorldStateReader struct {
	snapshot dstruntime.WorldStateSnapshot
	err      error
	calls    int
}

func (r *runtimeWorldStateReader) ReadWorldState(context.Context, string, string) (dstruntime.WorldStateSnapshot, error) {
	r.calls++
	return r.snapshot, r.err
}

type countingWorldStateSampler struct {
	observation Observation
	err         error
	calls       int
}

func (s *countingWorldStateSampler) Snapshot(context.Context, string, string) (Observation, error) {
	s.calls++
	return s.observation, s.err
}

func TestRuntimeSamplerPreservesEveryMetricWithoutCallingFallback(t *testing.T) {
	integer := func(value int) *int { return &value }
	number := func(value float64) *float64 { return &value }
	reader := &runtimeWorldStateReader{snapshot: dstruntime.WorldStateSnapshot{
		Season: "autumn", Phase: "day", Cycles: integer(48), ElapsedDaysInSeason: integer(6), RemainingDaysInSeason: integer(14),
		SeasonProgress: number(.3), DayProgress: number(.34), PhaseProgress: number(.57), Precipitation: "acid_rain", MoonPhase: "new",
		Temperature: number(18.5), Wetness: number(.18), Moisture: number(18), MoistureCeil: number(100), PrecipitationRate: number(.25),
		NightmarePhase: "warn", NightmareProgress: number(.46),
		CapturedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
	}}
	fallback := &countingWorldStateSampler{err: errors.New("fallback must not be called")}
	sampler, err := NewRuntimeSampler(reader, fallback)
	if err != nil {
		t.Fatal(err)
	}
	value, err := sampler.Snapshot(context.Background(), "room", "world")
	if err != nil || fallback.calls != 0 || value.Cycles == nil || *value.Cycles != 48 || value.ElapsedDaysInSeason == nil || *value.ElapsedDaysInSeason != 6 || value.RemainingDaysInSeason == nil || *value.RemainingDaysInSeason != 14 || value.SeasonProgress == nil || *value.SeasonProgress != .3 || value.DayProgress == nil || *value.DayProgress != .34 || value.PhaseProgress == nil || *value.PhaseProgress != .57 || value.Precipitation != "acid_rain" || value.MoonPhase != "new" || value.Temperature == nil || *value.Temperature != 18.5 || value.Wetness == nil || *value.Wetness != .18 || value.Moisture == nil || *value.Moisture != 18 || value.MoistureCeil == nil || *value.MoistureCeil != 100 || value.PrecipitationRate == nil || *value.PrecipitationRate != .25 || value.NightmarePhase != "warn" || value.NightmareProgress == nil || *value.NightmareProgress != .46 || !value.CapturedAt.Equal(reader.snapshot.CapturedAt) {
		t.Fatalf("runtime observation = %#v, fallback calls = %d, error = %v", value, fallback.calls, err)
	}
}

func TestRuntimeSamplerFallsBackExactlyOnceForUnavailableSnapshot(t *testing.T) {
	reader := &runtimeWorldStateReader{err: dstruntime.ErrSnapshotUnavailable}
	fallback := &countingWorldStateSampler{observation: Observation{Season: "winter"}}
	sampler, err := NewRuntimeSampler(reader, fallback)
	if err != nil {
		t.Fatal(err)
	}
	value, err := sampler.Snapshot(context.Background(), "room", "world")
	if err != nil || reader.calls != 1 || fallback.calls != 1 || value.Season != "winter" {
		t.Fatalf("fallback observation = %#v, reader calls = %d, fallback calls = %d, error = %v", value, reader.calls, fallback.calls, err)
	}
}

func TestRuntimeSamplerDoesNotFallbackAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &runtimeWorldStateReader{err: context.Canceled}
	fallback := &countingWorldStateSampler{}
	sampler, err := NewRuntimeSampler(reader, fallback)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sampler.Snapshot(ctx, "room", "world"); !errors.Is(err, context.Canceled) || fallback.calls != 0 {
		t.Fatalf("canceled snapshot error = %v, fallback calls = %d", err, fallback.calls)
	}
}
