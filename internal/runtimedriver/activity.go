package runtimedriver

import (
	"context"
	"encoding/json"
)

// Scope changes invalidate only this world's activity cursor. Placement is
// checked again after reading segments so evidence cannot mix two machines.
func (r *Router) ActivityScope(ctx context.Context, room, world string) (string, error) {
	_, target, err := r.DriverTarget(ctx, room, world)
	if err != nil {
		return "", err
	}
	value, _ := json.Marshal([]string{target.TargetID, target.InstallationID, target.Cluster, target.Shard})
	return string(value), nil
}
