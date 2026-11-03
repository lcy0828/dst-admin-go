package console

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/google/uuid"
)

type workbenchCallerKey struct{}

// WithWorkbenchCaller is set from authenticated server context, never arguments.
func WithWorkbenchCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, workbenchCallerKey{}, caller)
}

func bindNearbyCaller(ctx context.Context, room, world, script string) (string, error) {
	caller, _ := ctx.Value(workbenchCallerKey{}).(string)
	if caller == "" {
		return "", ErrInvalidArguments
	}
	scope := fmt.Sprintf("%x", sha256.Sum256([]byte(caller+"\x00"+room+"\x00"+world)))
	return "local _dst_admin_batch_caller=" + quoteLua(scope) + ";" + script, nil
}

func guardedNearby(action string) bool {
	if nearbyV2(action) {
		return true
	}
	switch action {
	case "extinguish", "freeze", "sleep", "pacify", "delete", "kill":
		return true
	}
	return false
}

func nearbyV2(action string) bool {
	switch action {
	case "pick", "repair", "fertilize", "grow", "ripen", "harvest", "repair_burnt":
		return true
	}
	return false
}

func nearbyPhase(args map[string]interface{}) string {
	phase, _ := args["phase"].(string)
	if phase == "" {
		return "preview"
	}
	return phase
}

// EffectiveWorkbenchRisk only narrows non-mutating preparation/cancellation of
// our fixed nearby commands. The legacy generic API keeps its confirmation.
func EffectiveWorkbenchRisk(d Definition, args map[string]interface{}) Risk {
	action, _ := args["action"].(string)
	if actionInteraction(d.ID, action) != nil && (nearbyPhase(args) == "preview" || nearbyPhase(args) == "cancel") {
		return RiskLow
	}
	return actionRisk(d.ID, action, d.Risk)
}

func renderGuardedNearby(args map[string]interface{}, playerID, action, prefab string, radius int) (string, error) {
	phase := nearbyPhase(args)
	if phase != "preview" && phase != "execute" && phase != "cancel" {
		return "", ErrInvalidArguments
	}
	token, _ := args["preview_id"].(string)
	if phase != "preview" {
		if _, err := uuid.Parse(token); err != nil {
			return "", ErrInvalidArguments
		}
	}
	if nearbyV2(action) {
		return fmt.Sprintf(`local r=rawget(_G,"DSTAdmin");local w=r and r.Commands and r.Commands.Workbench;if not w or not w.NearbyV2 or w.ResultVersion~=2 then error("WORKBENCH_UPDATE_REQUIRED")end;w.NearbyV2(%s,%s,%d,%s,%s,%s,%s,_dst_admin_batch_caller,_dst_admin_batch_request)`, quoteLua(playerID), quoteLua(action), radius, quoteLua(prefab), quoteLua(phase), quoteLua(token), quoteLua(uuid.NewString())), nil
	}
	return fmt.Sprintf(`local r=rawget(_G,"DSTAdmin");local w=r and r.Commands and r.Commands.Workbench;if not w or not w.Nearby then error("WORKBENCH_UPDATE_REQUIRED")end;w.Nearby(%s,%s,%d,%s,%s,%s,%s,_dst_admin_batch_caller)`, quoteLua(playerID), quoteLua(action), radius, quoteLua(prefab), quoteLua(phase), quoteLua(token), quoteLua(uuid.NewString())), nil
}
