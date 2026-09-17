package worldstate

import (
	"context"
	"errors"

	"dont/internal/dstruntime"
	"dont/internal/shards"
)

type RuntimeSnapshotReader interface {
	ReadWorldState(context.Context, string, string) (dstruntime.WorldStateSnapshot, error)
}

type RuntimeSampler struct {
	runtime RuntimeSnapshotReader
}

func NewRuntimeSampler(runtime RuntimeSnapshotReader) (*RuntimeSampler, error) {
	if runtime == nil {
		return nil, errors.New("runtime reader is required")
	}
	return &RuntimeSampler{runtime: runtime}, nil
}

func (s *RuntimeSampler) Snapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	return s.CurrentSnapshot(ctx, roomID, worldID)
}

// CurrentSnapshot reads only the managed runtime output and never invokes the
// console fallback. It is suitable for read-only current-state views.
func (s *RuntimeSampler) CurrentSnapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	snapshot, err := s.runtime.ReadWorldState(ctx, roomID, worldID)
	if err != nil {
		return Observation{}, err
	}
	return observationFromRuntime(snapshot), nil
}

func (s *RuntimeSampler) CurrentWorld(ctx context.Context, roomID, worldID string) (Observation, shards.RuntimeStatus, error) {
	reader, ok := s.runtime.(interface {
		ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error)
	})
	if !ok {
		return Observation{}, shards.RuntimeStatus{}, dstruntime.ErrWorldStateReadUnsupported
	}
	value, err := reader.ReadCurrentWorldState(ctx, roomID, worldID)
	status := shards.RuntimeStatus{
		State: shards.RuntimeState(value.Runtime.State), Code: value.Runtime.Code,
		Message: value.Runtime.Message, SessionExists: value.Runtime.SessionExists, Paused: value.Runtime.Paused,
	}
	return observationFromRuntime(value.Snapshot), status, err
}

// StoppedSnapshot never refreshes the runtime or sends console input.
func (s *RuntimeSampler) StoppedSnapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	reader, ok := s.runtime.(interface {
		ReadStoppedWorldState(context.Context, string, string) (dstruntime.WorldStateSnapshot, error)
	})
	if !ok {
		return Observation{}, dstruntime.ErrSnapshotUnavailable
	}
	snapshot, err := reader.ReadStoppedWorldState(ctx, roomID, worldID)
	if err != nil {
		return Observation{}, err
	}
	return observationFromRuntime(snapshot), nil
}

func observationFromRuntime(snapshot dstruntime.WorldStateSnapshot) Observation {
	return Observation{
		Season: snapshot.Season, Phase: snapshot.Phase, Cycles: snapshot.Cycles,
		ElapsedDaysInSeason: snapshot.ElapsedDaysInSeason, RemainingDaysInSeason: snapshot.RemainingDaysInSeason,
		SeasonProgress: snapshot.SeasonProgress, DayProgress: snapshot.DayProgress, PhaseProgress: snapshot.PhaseProgress,
		Precipitation: snapshot.Precipitation, MoonPhase: snapshot.MoonPhase, Temperature: snapshot.Temperature,
		Wetness: snapshot.Wetness, Moisture: snapshot.Moisture, MoistureCeil: snapshot.MoistureCeil,
		PrecipitationRate: snapshot.PrecipitationRate, NightmarePhase: snapshot.NightmarePhase,
		NightmareProgress: snapshot.NightmareProgress, HostPerformance: snapshot.HostPerformance,
		CapturedAt: snapshot.CapturedAt,
	}
}

var _ Sampler = (*RuntimeSampler)(nil)
