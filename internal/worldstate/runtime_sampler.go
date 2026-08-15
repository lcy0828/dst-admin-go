package worldstate

import (
	"context"
	"errors"

	"dont/internal/dstruntime"
)

type RuntimeSnapshotReader interface {
	ReadWorldState(context.Context, string, string) (dstruntime.WorldStateSnapshot, error)
}

type runtimeSnapshotRefresher interface {
	RefreshSnapshots(context.Context, string, string) (dstruntime.SnapshotRefreshResult, error)
}

type RuntimeSampler struct {
	runtime  RuntimeSnapshotReader
	fallback Sampler
}

func NewRuntimeSampler(runtime RuntimeSnapshotReader, fallback Sampler) (*RuntimeSampler, error) {
	if runtime == nil || fallback == nil {
		return nil, errors.New("runtime reader and fallback sampler are required")
	}
	return &RuntimeSampler{runtime: runtime, fallback: fallback}, nil
}

func (s *RuntimeSampler) Snapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	observation, err := s.CurrentSnapshot(ctx, roomID, worldID)
	if err == nil {
		return observation, nil
	}
	if contextErr := contextError(ctx, err); contextErr != nil {
		return Observation{}, contextErr
	}
	return s.fallback.Snapshot(ctx, roomID, worldID)
}

// CurrentSnapshot reads only the managed runtime output and never invokes the
// console fallback. It is suitable for read-only current-state views.
func (s *RuntimeSampler) CurrentSnapshot(ctx context.Context, roomID, worldID string) (Observation, error) {
	snapshot, err := s.runtime.ReadWorldState(ctx, roomID, worldID)
	if err != nil && refreshableSnapshotError(err) {
		if refresher, ok := s.runtime.(runtimeSnapshotRefresher); ok {
			refreshed, refreshErr := refresher.RefreshSnapshots(ctx, roomID, worldID)
			if refreshErr == nil {
				snapshot = refreshed.WorldState
				err = nil
			} else {
				err = errors.Join(err, refreshErr)
			}
		}
	}
	if err != nil {
		return Observation{}, err
	}
	return observationFromRuntime(snapshot), nil
}

func refreshableSnapshotError(err error) bool {
	return errors.Is(err, dstruntime.ErrSnapshotUnavailable) || errors.Is(err, dstruntime.ErrSnapshotStale)
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func observationFromRuntime(snapshot dstruntime.WorldStateSnapshot) Observation {
	return Observation{
		Season: snapshot.Season, Phase: snapshot.Phase, Cycles: snapshot.Cycles,
		ElapsedDaysInSeason: snapshot.ElapsedDaysInSeason, RemainingDaysInSeason: snapshot.RemainingDaysInSeason,
		SeasonProgress: snapshot.SeasonProgress, DayProgress: snapshot.DayProgress, PhaseProgress: snapshot.PhaseProgress,
		Precipitation: snapshot.Precipitation, MoonPhase: snapshot.MoonPhase, Temperature: snapshot.Temperature,
		Wetness: snapshot.Wetness, Moisture: snapshot.Moisture, MoistureCeil: snapshot.MoistureCeil,
		PrecipitationRate: snapshot.PrecipitationRate, NightmarePhase: snapshot.NightmarePhase,
		NightmareProgress: snapshot.NightmareProgress, CapturedAt: snapshot.CapturedAt,
	}
}

var _ Sampler = (*RuntimeSampler)(nil)
