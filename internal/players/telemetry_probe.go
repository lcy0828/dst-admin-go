package players

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	SessionID    string
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

type placementLocality interface {
	IsLocalPlacement(string, string) (bool, error)
}

var ErrTelemetryRecoveryDeferred = errors.New("runtime telemetry recovery is cooling down or already in progress")

const maxTelemetryRecoveries = 1024

type telemetryRecoveryKey struct{ roomID, worldID string }
type telemetryRecovery struct {
	active   bool
	failures int
	retryAt  time.Time
}

type TelemetryProbe struct {
	native         Probe
	runtime        RuntimeSnapshotReader
	fallback       Probe
	remoteFallback Probe
	history        HistoryProbe
	locality       placementLocality
	now            func() time.Time
	recoveryMu     sync.Mutex
	recoveries     map[telemetryRecoveryKey]*telemetryRecovery
}

func NewTelemetryProbe(native Probe, runtime RuntimeSnapshotReader, fallback Probe, localities ...placementLocality) (*TelemetryProbe, error) {
	if native == nil || runtime == nil || fallback == nil {
		return nil, errors.New("native, runtime, and fallback probes are required")
	}
	probe := &TelemetryProbe{native: native, runtime: runtime, fallback: fallback, now: time.Now}
	if len(localities) > 0 {
		probe.locality = localities[0]
	}
	return probe, nil
}

func (p *TelemetryProbe) Snapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	result, err := p.SnapshotDetailed(ctx, roomID, worldID)
	return result.Observations, err
}

func (p *TelemetryProbe) ConfigureRemoteFallback(probe Probe) {
	p.remoteFallback = probe
}

func (p *TelemetryProbe) ConfigureHistory(probe HistoryProbe) {
	p.history = probe
}

func (p *TelemetryProbe) SnapshotDetailed(ctx context.Context, roomID, worldID string) (SnapshotResult, error) {
	local, err := p.isLocal(roomID, worldID)
	if err != nil {
		return SnapshotResult{}, err
	}
	var native []Observation
	var nativeErr error
	if local {
		native, nativeErr = p.native.Snapshot(ctx, roomID, worldID)
	}
	nativeAt := p.now().UTC()
	native = stampNativeObservations(native, nativeAt)
	snapshot, runtimeErr := p.runtime.ReadPlayers(ctx, roomID, worldID)
	key := telemetryRecoveryKey{roomID, worldID}
	if runtimeErr == nil {
		p.resetRecovery(key)
	} else {
		finish, allowed := p.beginRecovery(key)
		if !allowed {
			return SnapshotResult{}, errors.Join(runtimeErr, ErrTelemetryRecoveryDeferred)
		}
		// Even a successful console fallback leaves the managed runtime broken.
		// Back off active probes, while continuing passive reads on every request.
		defer func() { finish(runtimeErr == nil || ctx.Err() != nil) }()
	}
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
		if snapshot.SessionID != "" {
			// Log identity can predate a world reset. Keep account identity,
			// but never fill a new save's missing character from old log data.
			for i, identity := range native {
				native[i] = Observation{ID: identity.ID, Name: identity.Name, NetID: identity.NetID,
					Fields: FieldStates{"name": identity.Fields["name"], "netId": identity.Fields["netId"]}}
			}
		}
		return SnapshotResult{
			Observations: mergeSnapshotIdentities(native, observations), Source: SourceRuntime, ObservedAt: snapshot.CapturedAt, SessionID: snapshot.SessionID,
			Degraded: nativeErr != nil, Warning: errorMessage(nativeErr),
		}, nil
	}
	fallbackProbe := p.fallback
	if !local {
		fallbackProbe = p.remoteFallback
	}
	if fallbackProbe == nil || !local && !refreshableSnapshotError(runtimeErr) || ctx.Err() != nil {
		return SnapshotResult{}, fmt.Errorf("remote runtime telemetry: %w", runtimeErr)
	}
	fallback, fallbackErr := fallbackProbe.Snapshot(ctx, roomID, worldID)
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

func (p *TelemetryProbe) resetRecovery(key telemetryRecoveryKey) {
	p.recoveryMu.Lock()
	defer p.recoveryMu.Unlock()
	if state := p.recoveries[key]; state != nil && !state.active {
		delete(p.recoveries, key)
	}
}

func (p *TelemetryProbe) beginRecovery(key telemetryRecoveryKey) (func(bool), bool) {
	p.recoveryMu.Lock()
	defer p.recoveryMu.Unlock()
	if p.recoveries == nil {
		p.recoveries = make(map[telemetryRecoveryKey]*telemetryRecovery)
	}
	now := p.now()
	state := p.recoveries[key]
	if state != nil && (state.active || now.Before(state.retryAt)) {
		return nil, false
	}
	if state == nil {
		if len(p.recoveries) >= maxTelemetryRecoveries {
			var oldestKey telemetryRecoveryKey
			var oldest *telemetryRecovery
			for candidate, value := range p.recoveries {
				if !value.active && (oldest == nil || value.retryAt.Before(oldest.retryAt)) {
					oldestKey, oldest = candidate, value
				}
			}
			if oldest == nil {
				return nil, false
			}
			delete(p.recoveries, oldestKey)
		}
		state = &telemetryRecovery{}
		p.recoveries[key] = state
	}
	state.active = true
	return func(recovered bool) {
		p.recoveryMu.Lock()
		defer p.recoveryMu.Unlock()
		if recovered {
			delete(p.recoveries, key)
			return
		}
		state.active = false
		delay := 30 * time.Second * time.Duration(1<<state.failures)
		if delay >= 5*time.Minute {
			delay = 5 * time.Minute
		} else {
			state.failures++
		}
		state.retryAt = p.now().Add(delay)
	}, true
}

func refreshableSnapshotError(err error) bool {
	return errors.Is(err, dstruntime.ErrSnapshotUnavailable) || errors.Is(err, dstruntime.ErrSnapshotStale)
}

func (p *TelemetryProbe) HistorySnapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	if p.history != nil {
		return p.history.HistorySnapshot(ctx, roomID, worldID)
	}
	local, err := p.isLocal(roomID, worldID)
	if err != nil {
		return nil, err
	}
	if !local {
		return []Observation{}, nil
	}
	if history, ok := p.native.(HistoryProbe); ok {
		return history.HistorySnapshot(ctx, roomID, worldID)
	}
	return []Observation{}, nil
}

func (p *TelemetryProbe) isLocal(roomID, worldID string) (bool, error) {
	if p.locality == nil {
		return true, nil
	}
	return p.locality.IsLocalPlacement(roomID, worldID)
}

func observationsFromRuntime(snapshot dstruntime.Snapshot) []Observation {
	result := make([]Observation, 0, len(snapshot.Players))
	for _, player := range snapshot.Players {
		observation := Observation{
			ID: player.ID, Name: player.Name, Prefab: player.Prefab, Admin: player.Admin, Age: player.Age,
			GameplayState: normalizeGameplayState(player.GameplayState),
			NetID:         player.NetID, NetScore: player.NetScore, HealthPercent: player.HealthPercent,
			HungerPercent: player.HungerPercent, SanityPercent: player.SanityPercent,
			Health: player.Health, HealthMax: player.HealthMax, Hunger: player.Hunger, HungerMax: player.HungerMax,
			Sanity: player.Sanity, SanityMax: player.SanityMax,
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
		for field, available := range map[string]bool{"prefab": value.Prefab != "", "netId": value.NetID != "", "gameplayState": false} {
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
	for field, available := range map[string]bool{"prefab": value.Prefab != "", "netId": value.NetID != "", "gameplayState": value.GameplayState != ""} {
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
		"health": value.Health != nil, "healthMax": value.HealthMax != nil,
		"hunger": value.Hunger != nil, "hungerMax": value.HungerMax != nil,
		"sanity": value.Sanity != nil, "sanityMax": value.SanityMax != nil,
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

func normalizeGameplayState(value string) string {
	switch value {
	case GameplayStateSelectingCharacter, GameplayStateLoading, GameplayStateAlive, GameplayStateDead,
		GameplayStateGhost, GameplayStateMigrating, GameplayStateUnknown:
		return value
	case "":
		return ""
	default:
		return GameplayStateUnknown
	}
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

// CurrentSession reads validated passive world state, including while DST is
// paused. It never refreshes telemetry or sends console commands.
func (p *TelemetryProbe) CurrentSession(ctx context.Context, roomID, worldID string) (string, time.Time, error) {
	reader, ok := p.runtime.(interface {
		ReadCurrentWorldState(context.Context, string, string) (dstruntime.CurrentWorldState, error)
	})
	if !ok {
		return "", time.Time{}, nil
	}
	value, err := reader.ReadCurrentWorldState(ctx, roomID, worldID)
	if err != nil {
		return "", time.Time{}, err
	}
	return value.Snapshot.SessionID, value.Snapshot.CapturedAt, nil
}
