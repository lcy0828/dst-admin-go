package mods

import "context"

// Persist only installation transitions, never every byte/progress sample.
// This is an in-process hook shared by local and Agent adapters, not a new RPC.
type ModUpdateTarget struct {
	TargetID       string   `json:"targetId"`
	InstallationID string   `json:"installationId"`
	ModIDs         []string `json:"modIds"`
	Status         string   `json:"status"`
	Message        string   `json:"message,omitempty"`
}

type updateTrackingKey struct{}
type updateTracking struct {
	previous []ModUpdateTarget
	record   func(ModUpdateTarget) error
}

func WithUpdateTracking(ctx context.Context, previous []ModUpdateTarget, record func(ModUpdateTarget) error) context.Context {
	return context.WithValue(ctx, updateTrackingKey{}, updateTracking{previous: previous, record: record})
}

func PreviousUpdateTarget(ctx context.Context, targetID, installationID string) *ModUpdateTarget {
	tracking, _ := ctx.Value(updateTrackingKey{}).(updateTracking)
	for _, target := range tracking.previous {
		if target.TargetID == targetID && target.InstallationID == installationID {
			return &target
		}
	}
	return nil
}

func TracksUpdate(ctx context.Context) bool {
	tracking, _ := ctx.Value(updateTrackingKey{}).(updateTracking)
	return tracking.record != nil
}

func RecordUpdateTarget(ctx context.Context, target ModUpdateTarget) error {
	tracking, _ := ctx.Value(updateTrackingKey{}).(updateTracking)
	if tracking.record == nil {
		return nil
	}
	return tracking.record(target)
}

type ModUpdateScope struct {
	Revision        string   `json:"revision"`
	AffectedRoomIDs []string `json:"affectedRoomIds"`
}
type updateScopeKey struct{}

func WithUpdateScope(ctx context.Context, revision string) context.Context {
	return context.WithValue(ctx, updateScopeKey{}, revision)
}
func ExpectedUpdateScope(ctx context.Context) string {
	value, _ := ctx.Value(updateScopeKey{}).(string)
	return value
}
