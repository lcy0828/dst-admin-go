package dstruntime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/shared"
)

var (
	ErrWorldStateReadUnsupported = errors.New("combined world state read is unavailable")
	ErrWorldStatePending         = errors.New("waiting for the first world state from the current runtime")
	ErrWorldStateAbsent          = errors.New("world has no saved state sample")
)

type CurrentWorldState struct {
	Runtime  shared.ShardRuntimeStatus
	Snapshot WorldStateSnapshot
}

// HealthMatchesCurrentProcess checks persisted health against the current
// Runtime's log identity. A save session alone cannot identify a process boot.
// This is an on-demand file read; it never wakes or writes to the game.
func (b *DistributedBridge) HealthMatchesCurrentProcess(ctx context.Context, roomID, worldID string, health Health) (bool, error) {
	reader, ok := b.runtime.(interface {
		ReadWorldState(context.Context, string, string) (shared.RuntimeWorldStateRead, error)
	})
	if !ok {
		return false, ErrWorldStateReadUnsupported
	}
	value, err := reader.ReadWorldState(ctx, roomID, worldID)
	if err != nil {
		return false, err
	}
	if value.ReadError != "" {
		return false, fmt.Errorf("%w: %s", ErrSnapshotUnavailable, value.ReadError)
	}
	if value.Runtime.State != "running" || value.Runtime.Paused == nil || !*value.Runtime.Paused ||
		value.StartedAt.IsZero() || value.StartedAt.After(b.now().UTC()) ||
		value.SessionID == "" || value.SessionID != health.SessionID ||
		value.ShardID == "" || value.ShardID != health.ShardID ||
		health.ProducerInstanceID == "" || health.LastCapturedAtUnix == nil || health.LastWrittenAtUnix == nil {
		return false, nil
	}
	startedAt := value.StartedAt.Truncate(time.Second)
	capturedAt := time.Unix(*health.LastCapturedAtUnix, 0)
	writtenAt := time.Unix(*health.LastWrittenAtUnix, 0)
	return !capturedAt.Before(startedAt) && !writtenAt.Before(capturedAt) &&
		!writtenAt.After(b.now().UTC().Add(maxFutureSkew)) && !health.ReadAt.Before(startedAt), nil
}

func (b *DistributedBridge) ReadCurrentWorldState(ctx context.Context, roomID, worldID string) (CurrentWorldState, error) {
	reader, ok := b.runtime.(interface {
		ReadWorldState(context.Context, string, string) (shared.RuntimeWorldStateRead, error)
	})
	if !ok {
		return CurrentWorldState{}, ErrWorldStateReadUnsupported
	}
	value, err := reader.ReadWorldState(ctx, roomID, worldID)
	result := CurrentWorldState{Runtime: value.Runtime}
	if err != nil {
		return result, err
	}
	switch value.Runtime.State {
	case "running", "stopped", "failed", "starting", "stopping", "unknown":
	default:
		return result, fmt.Errorf("%w: invalid runtime state", ErrSnapshotInvalid)
	}
	if len(value.SessionID) > 128 || len(value.ShardID) > 64 || len(value.ReadError) > 4096 ||
		strings.ContainsAny(value.SessionID+value.ShardID, "\x00\r\n") || value.StartedAt.After(b.now().UTC().Add(maxFutureSkew)) {
		return result, ErrSnapshotInvalid
	}
	if value.ReadError != "" {
		return result, fmt.Errorf("%w: %s", ErrSnapshotUnavailable, value.ReadError)
	}
	if (value.Runtime.State == "stopped" || value.Runtime.State == "failed") && value.Artifacts.Kind == shared.ArtifactRuntimeWorldState && len(value.Artifacts.Artifacts) == 0 {
		return result, ErrWorldStateAbsent
	}
	if value.Runtime.State == "starting" || value.Runtime.State == "running" &&
		(value.StartedAt.IsZero() || value.Artifacts.Kind == shared.ArtifactRuntimeWorldState && len(value.Artifacts.Artifacts) == 0) {
		return result, ErrWorldStatePending
	}
	artifacts, err := validatedArtifacts(value.Artifacts, shared.ArtifactRuntimeWorldState, "worldstate-a.json", "worldstate-b.json")
	if err != nil {
		return result, err
	}
	candidates := make([]WorldStateSnapshot, 0, len(artifacts))
	var failures error
	for _, artifact := range artifacts {
		snapshot, decodeErr := decodeWorldStateData(artifact.Data, value.SessionID, value.ShardID, b.now().UTC(), false)
		if decodeErr != nil {
			failures = errors.Join(failures, fmt.Errorf("%s: %w", artifact.Name, decodeErr))
			continue
		}
		// A DST save session survives process restarts. The current log's startup
		// time prevents the previous process' last JSON from appearing live.
		if !value.StartedAt.IsZero() && snapshot.CapturedAt.Before(value.StartedAt) {
			failures = errors.Join(failures, ErrWorldStatePending)
			continue
		}
		candidates = append(candidates, snapshot)
	}
	if len(candidates) == 0 {
		return result, errors.Join(ErrSnapshotUnavailable, failures)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.ProducerInstanceID == right.ProducerInstanceID && left.Sequence != right.Sequence {
			return left.Sequence > right.Sequence
		}
		return left.CapturedAt.After(right.CapturedAt)
	})
	result.Snapshot = candidates[0]
	return result, nil
}
