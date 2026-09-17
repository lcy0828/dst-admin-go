package players

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dont/internal/runtimefiles"
	"dont/shared"
)

type historyArtifactReader interface {
	ReadArtifacts(context.Context, string, string, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error)
}

type RuntimeHistoryProbe struct{ runtime historyArtifactReader }

var ErrHistoryCatchingUp = errors.New("历史记录正在分批补齐，将在后续刷新继续读取")

func NewRuntimeHistoryProbe(runtime historyArtifactReader) *RuntimeHistoryProbe {
	return &RuntimeHistoryProbe{runtime: runtime}
}

func (p *RuntimeHistoryProbe) HistorySnapshot(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	bundle, err := p.runtime.ReadArtifacts(ctx, roomID, worldID, shared.ArtifactRuntimePlayerHistory)
	if err != nil {
		return nil, err
	}
	if err := runtimefiles.ValidateArtifactBundle(shared.ArtifactRuntimePlayerHistory, bundle); err != nil {
		return nil, err
	}
	var history runtimefiles.PlayerHistoryResult
	if err := json.Unmarshal(bundle.Artifacts[0].Data, &history); err != nil {
		return nil, err
	}
	if len(history.Players) > runtimefiles.MaximumHistoricalPlayers {
		return nil, errors.New("player history exceeds identity limit")
	}
	result := make([]Observation, 0, len(history.Players))
	for _, player := range history.Players {
		if player.FirstSeenAt.IsZero() || player.LastSeenAt.Before(player.FirstSeenAt) || player.LastSeenAt.After(time.Now().Add(30*time.Second)) ||
			player.LastConnectedAt.After(player.LastSeenAt) || player.LastDisconnectedAt.After(player.LastSeenAt) || player.PrefabObservedAt.After(player.LastSeenAt) {
			return nil, errors.New("player history contains invalid event times")
		}
		fields := FieldStates{}
		if player.Prefab != "" && !player.PrefabObservedAt.IsZero() {
			fields["prefab"] = historicalField(player.PrefabObservedAt)
		}
		result = append(result, Observation{
			HistoryWorldName: player.LastSeenWorld,
			ID:               player.ID, Name: player.Name, Prefab: player.Prefab, NetID: player.NetID, Fields: fields,
			FirstSeenAt: player.FirstSeenAt, LastSeenAt: player.LastSeenAt,
			LastConnectedAt: player.LastConnectedAt, LastDisconnectedAt: player.LastDisconnectedAt,
		})
	}
	// A bounded archive scan resumes at its cursor on the next refresh. Return
	// the evidence already read instead of discarding recent connections.
	if !history.Complete {
		return result, ErrHistoryCatchingUp
	}
	return result, nil
}

func historicalField(at time.Time) FieldState {
	instant := at.UTC()
	return FieldState{Source: SourceNativeLog, ObservedAt: &instant, Status: FreshnessStale}
}
