package players

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/dstruntime"
)

type DataSource string

const (
	SourceRuntime         DataSource = "customcommands"
	SourceConsoleFallback DataSource = "console-fallback"
	SourceNativeLog       DataSource = "native-log"
)

type SnapshotResult struct {
	Observations []Observation
	Source       DataSource
	ObservedAt   time.Time
	Degraded     bool
	Warning      string
}

type DetailedProbe interface {
	SnapshotDetailed(context.Context, string, string) (SnapshotResult, error)
}

type RuntimeSnapshotReader interface {
	ReadPlayers(context.Context, string, string) (dstruntime.Snapshot, error)
}

type runtimeSnapshotRefresher interface {
	RefreshSnapshots(context.Context, string, string) (dstruntime.SnapshotRefreshResult, error)
}

type TelemetryProbe struct {
	native   Probe
	runtime  RuntimeSnapshotReader
	fallback Probe
	now      func() time.Time
}

func NewTelemetryProbe(native Probe, runtime RuntimeSnapshotReader, fallback Probe) (*TelemetryProbe, error) {
	if native == nil || runtime == nil || fallback == nil {
		return nil, errors.New("native, runtime, and fallback probes are required")
	}
	return &TelemetryProbe{native: native, runtime: runtime, fallback: fallback, now: time.Now}, nil
}

func (p *TelemetryProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	result, err := p.SnapshotDetailed(ctx, roomID, worldID)
	return result.Observations, err
}

func (p *TelemetryProbe) SnapshotDetailed(ctx context.Context, roomID, worldID string) (SnapshotResult, error) {
	native, nativeErr := p.native.Snapshot(ctx, roomID, worldID)
	nativeAt := p.now().UTC()
	native = stampNativeObservations(native, nativeAt)
	snapshot, runtimeErr := p.runtime.ReadPlayers(ctx, roomID, worldID)
	if runtimeErr != nil && refreshableSnapshotError(runtimeErr) {
		if refresher, ok := p.runtime.(runtimeSnapshotRefresher); ok {
			refreshed, refreshErr := refresher.RefreshSnapshots(ctx, roomID, worldID)
			if refreshErr == nil {
				snapshot = refreshed.Players
				runtimeErr = nil
			} else {
				runtimeErr = errors.Join(runtimeErr, fmt.Errorf("active runtime refresh: %w", refreshErr))
			}
		}
	}
	if runtimeErr == nil {
		observations := observationsFromRuntime(snapshot)
		return SnapshotResult{
			Observations: mergeSnapshotIdentities(native, observations), Source: SourceRuntime, ObservedAt: snapshot.CapturedAt,
			Degraded: nativeErr != nil, Warning: errorMessage(nativeErr),
		}, nil
	}
	fallback, fallbackErr := p.fallback.Snapshot(ctx, roomID, worldID)
	if fallbackErr != nil {
		return SnapshotResult{}, errors.Join(fmt.Errorf("runtime telemetry: %w", runtimeErr), fmt.Errorf("console fallback: %w", fallbackErr), nativeErr)
	}
	warning := "customcommands 采集不可用，本次已使用控制台 fallback：" + runtimeErr.Error()
	if nativeErr != nil {
		warning += "；原生日志读取失败：" + nativeErr.Error()
	}
	fallbackAt := p.now().UTC()
	fallback = stampTelemetryObservations(fallback, SourceConsoleFallback, fallbackAt)
	return SnapshotResult{
		Observations: mergeSnapshotIdentities(native, fallback), Source: SourceConsoleFallback, ObservedAt: fallbackAt, Degraded: true, Warning: warning,
	}, nil
}

func refreshableSnapshotError(err error) bool {
	return errors.Is(err, dstruntime.ErrSnapshotUnavailable) || errors.Is(err, dstruntime.ErrSnapshotStale)
}

func (p *TelemetryProbe) HistorySnapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	if history, ok := p.native.(HistoryProbe); ok {
		return history.HistorySnapshot(ctx, roomID, worldID)
	}
	return []Observation{}, nil
}

func observationsFromRuntime(snapshot dstruntime.Snapshot) []Observation {
	result := make([]Observation, 0, len(snapshot.Players))
	for _, player := range snapshot.Players {
		observation := Observation{
			ID: player.ID, Name: player.Name, Prefab: player.Prefab, Admin: player.Admin, Age: player.Age,
			NetID: player.NetID, NetScore: player.NetScore, HealthPercent: player.HealthPercent,
			HungerPercent: player.HungerPercent, SanityPercent: player.SanityPercent,
			Temperature: player.Temperature, Moisture: player.Moisture,
		}
		result = append(result, stampTelemetryObservation(observation, SourceRuntime, snapshot.CapturedAt))
	}
	return result
}

func stampNativeObservations(values []Observation, observedAt time.Time) []Observation {
	result := make([]Observation, 0, len(values))
	for _, value := range values {
		if value.Fields == nil {
			value.Fields = make(FieldStates)
		}
		for _, field := range []string{"online", "name", "admin", "age"} {
			value.Fields[field] = liveField(SourceNativeLog, observedAt)
		}
		for field, available := range map[string]bool{"prefab": value.Prefab != "", "netId": value.NetID != ""} {
			status := FreshnessUnavailable
			if available {
				status = FreshnessLive
			}
			instant := observedAt.UTC()
			value.Fields[field] = FieldState{Source: SourceNativeLog, ObservedAt: &instant, Status: status}
		}
		result = append(result, value)
	}
	return result
}

func stampTelemetryObservations(values []Observation, source DataSource, observedAt time.Time) []Observation {
	result := make([]Observation, 0, len(values))
	for _, value := range values {
		result = append(result, stampTelemetryObservation(value, source, observedAt))
	}
	return result
}

func stampTelemetryObservation(value Observation, source DataSource, observedAt time.Time) Observation {
	if value.Fields == nil {
		value.Fields = make(FieldStates)
	}
	for _, field := range []string{"online", "name", "admin", "age"} {
		value.Fields[field] = liveField(source, observedAt)
	}
	for field, available := range map[string]bool{"prefab": value.Prefab != "", "netId": value.NetID != ""} {
		status := FreshnessUnavailable
		if available {
			status = FreshnessLive
		}
		instant := observedAt.UTC()
		value.Fields[field] = FieldState{Source: source, ObservedAt: &instant, Status: status}
	}
	metrics := map[string]bool{
		"netScore": value.NetScore != nil, "healthPercent": value.HealthPercent != nil,
		"hungerPercent": value.HungerPercent != nil, "sanityPercent": value.SanityPercent != nil,
		"temperature": value.Temperature != nil, "moisture": value.Moisture != nil,
	}
	for field, available := range metrics {
		status := FreshnessUnavailable
		if available {
			status = FreshnessLive
		}
		instant := observedAt.UTC()
		value.Fields[field] = FieldState{Source: source, ObservedAt: &instant, Status: status}
	}
	return value
}

func liveField(source DataSource, observedAt time.Time) FieldState {
	instant := observedAt.UTC()
	return FieldState{Source: source, ObservedAt: &instant, Status: FreshnessLive}
}

func mergeSnapshotIdentities(identity, snapshot []Observation) []Observation {
	identities := make(map[string]Observation, len(identity))
	for _, observation := range identity {
		identities[observation.ID] = mergeObservation(identities[observation.ID], observation)
	}
	result := make([]Observation, 0, len(snapshot))
	for _, observation := range snapshot {
		merged := mergeObservation(identities[observation.ID], observation)
		// A runtime/fallback snapshot is authoritative for values where zero or
		// false are valid. Native logs only fill identity details missing there.
		merged.Admin = observation.Admin
		merged.Age = observation.Age
		for _, field := range []string{"prefab", "netId"} {
			if state, exists := observation.Fields[field]; exists && state.Status == FreshnessUnavailable {
				if identityState, identityExists := identities[observation.ID].Fields[field]; identityExists {
					merged.Fields[field] = identityState
				}
			}
		}
		result = append(result, merged)
	}
	return result
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ Probe = (*TelemetryProbe)(nil)
var _ DetailedProbe = (*TelemetryProbe)(nil)
var _ HistoryProbe = (*TelemetryProbe)(nil)
