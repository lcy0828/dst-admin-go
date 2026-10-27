package players

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dont/internal/dstruntime"
)

// InventorySnapshot avoids log/history scans. It only actively samples when
// shard-local evidence is missing, too old for routing, or explicitly refreshed.
func (p *TelemetryProbe) InventorySnapshot(ctx context.Context, roomID, worldID string, force bool) (SnapshotResult, error) {
	local, err := p.isLocal(roomID, worldID)
	if err != nil {
		return SnapshotResult{}, err
	}
	snapshot, readErr := p.runtime.ReadPlayers(ctx, roomID, worldID)
	if readErr == nil {
		p.resetRecovery(telemetryRecoveryKey{roomID, worldID})
	}
	result := inventoryRuntimeSnapshot(snapshot)
	if !force && readErr == nil && inventorySnapshotFresh(result, p.now()) {
		return result, nil
	}
	// A disconnected remote target must never fall back to controller files.
	if !local && readErr != nil && !refreshableSnapshotError(readErr) {
		return SnapshotResult{}, readErr
	}
	finish, allowed := p.beginRecovery(telemetryRecoveryKey{roomID, worldID})
	if !allowed {
		return SnapshotResult{}, ErrTelemetryRecoveryDeferred
	}
	recovered := false
	defer func() { finish(recovered || ctx.Err() != nil) }()
	if refresher, ok := p.runtime.(runtimeSnapshotRefresher); ok && (readErr == nil || refreshableSnapshotError(readErr)) {
		refreshed, refreshErr := refresher.RefreshSnapshots(ctx, roomID, worldID)
		if refreshErr == nil {
			result = inventoryRuntimeSnapshot(refreshed.Players)
			if inventorySnapshotFresh(result, p.now()) {
				recovered = true
				return result, nil
			}
		} else {
			readErr = errors.Join(readErr, refreshErr)
		}
	}
	if ctx.Err() != nil {
		return SnapshotResult{}, ctx.Err()
	}
	fallback := p.fallback
	if !local {
		fallback = p.remoteFallback
	}
	if fallback == nil {
		return SnapshotResult{}, fmt.Errorf("local player evidence unavailable: %w", dstruntime.ErrSnapshotUnavailable)
	}
	observations, fallbackErr := fallback.Snapshot(ctx, roomID, worldID)
	if fallbackErr != nil {
		return SnapshotResult{}, errors.Join(readErr, fallbackErr)
	}
	if !observationsHaveLocality(observations) {
		return SnapshotResult{}, dstruntime.ErrSnapshotUnavailable
	}
	recovered = true
	now := p.now().UTC()
	return SnapshotResult{
		LocalityKnown: true, Observations: stampTelemetryObservations(observations, SourceConsoleFallback, now),
		Source: SourceConsoleFallback, ObservedAt: now,
	}, nil
}

func inventoryRuntimeSnapshot(snapshot dstruntime.Snapshot) SnapshotResult {
	return SnapshotResult{LocalityKnown: snapshot.LocalityKnown, Observations: observationsFromRuntime(snapshot),
		Source: SourceRuntime, ObservedAt: snapshot.CapturedAt, SessionID: snapshot.SessionID}
}

func observationsHaveLocality(observations []Observation) bool {
	for _, observation := range observations {
		if observation.LocalEntity == nil {
			return false
		}
	}
	return true
}

func inventorySnapshotFresh(result SnapshotResult, now time.Time) bool {
	age := now.Sub(result.ObservedAt)
	return result.LocalityKnown && !result.Degraded && age >= -2*time.Second && age <= 15*time.Second && observationsHaveLocality(result.Observations)
}
