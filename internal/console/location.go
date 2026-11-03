package console

import (
	"context"
	"encoding/json"
	"fmt"

	"dont/internal/dstruntime"
	"github.com/google/uuid"
)

type LocationIdentity struct{ SessionID, Instance string }

func (s *Service) LocationIdentity(ctx context.Context, room, world string) (LocationIdentity, error) {
	bridge, ok := s.commander.(interface {
		QueryIdentity(context.Context, string, string) (string, error)
		Health(context.Context, string, string) (dstruntime.Health, error)
	})
	if !ok {
		return LocationIdentity{}, dstruntime.ErrRuntimeUnavailable
	}
	identity, e := bridge.QueryIdentity(ctx, room, world)
	if e != nil {
		return LocationIdentity{}, e
	}
	h, e := bridge.Health(ctx, room, world)
	if e != nil {
		return LocationIdentity{}, e
	}
	if h.SessionID == "" || h.ProducerVersion != dstruntime.RuntimeVersion {
		return LocationIdentity{}, dstruntime.ErrRuntimeUnavailable
	}
	return LocationIdentity{SessionID: h.SessionID, Instance: identity}, nil
}

// ExecuteLocation is internal-only: there is no editable definition or HTTP
// script input for this command. The coordinator supplies the durable run ID.
func (s *Service) ExecuteLocation(ctx context.Context, roomID, worldID, runID string, args map[string]any) (Run, error) {
	if _, e := uuid.Parse(runID); e != nil || s.commander == nil {
		return Run{}, ErrInvalidArguments
	}
	room, world, e := s.resolve(roomID, worldID)
	if e != nil {
		return Run{}, e
	}
	raw, e := json.Marshal(args)
	if e != nil || len(raw) > 2048 {
		return Run{}, ErrInvalidArguments
	}
	script := fmt.Sprintf(`local r=rawget(_G,"DSTAdmin");local w=r and r.Commands and r.Commands.Workbench;if not w or not w.Teleport then error("WORKBENCH_UPDATE_REQUIRED")end;w.Teleport(require("json").decode(%s))`, quoteLua(string(raw)))
	run, e := s.store.createWithID(Run{RoomID: room.ID, WorldID: world.ID, Mode: "location", CommandID: "location_teleport", Name: "地点与传送", Risk: RiskCritical, Arguments: args, Script: script}, runID)
	if e != nil {
		return Run{}, e
	} // a duplicate ID must never dispatch again
	return s.executeVerified(ctx, room, world, run, script)
}
