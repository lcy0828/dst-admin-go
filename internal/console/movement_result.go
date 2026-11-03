package console

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/google/uuid"
)

type MovementPoint struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

func (p *MovementPoint) valid() bool {
	return p != nil && !math.IsNaN(p.X) && !math.IsNaN(p.Z) && math.Abs(p.X) <= 100000 && math.Abs(p.Z) <= 100000
}

type MovementItem struct {
	PlayerName string         `json:"playerName,omitempty"`
	PlayerID   string         `json:"playerId"`
	State      string         `json:"state"`
	Reason     string         `json:"reason,omitempty"`
	Point      *MovementPoint `json:"point"`
	Before     *MovementPoint `json:"before,omitempty"`
	After      *MovementPoint `json:"after,omitempty"`
}
type MovementResult struct {
	PreviewID  string         `json:"previewId,omitempty"`
	ExpiresAt  int64          `json:"expiresAt,omitempty"`
	State      string         `json:"state"`
	Attempted  int            `json:"attempted"`
	Verified   int            `json:"verified"`
	ObservedAt int64          `json:"observedAt"`
	Items      []MovementItem `json:"items"`
}

func movementCommand(id string) bool {
	return id == "teleport_player" || id == "relocate_players" || id == "teleport_to_nearest_entity"
}

// Only fixed movement commands and the same verified receipt can establish an
// effect. Generic console output and an old transport-only "sent" cannot.
func movementResult(run Run) *MovementResult {
	if run.Mode != "builtin" || !movementCommand(run.CommandID) || run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated || !strings.Contains(run.Script, "w.MovePlayers(") {
		return nil
	}
	var result *MovementResult
	for _, line := range strings.Split(run.Output.Text, "\n") {
		const prefix = "[DST-ADMIN-MOVE-V1] "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if result != nil || len(line) > 16384 {
			return nil
		}
		var r MovementResult
		if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &r) != nil || r.ObservedAt <= 0 || len(r.Items) > 40 || r.Attempted < 0 || r.Attempted > len(r.Items) {
			return nil
		}
		seen, verified, attempted := map[string]bool{}, 0, 0
		for _, item := range r.Items {
			if !dstUserIDPattern.MatchString(item.PlayerID) || seen[item.PlayerID] || !item.Point.valid() || item.Before != nil && !item.Before.valid() || item.After != nil && !item.After.valid() {
				return nil
			}
			seen[item.PlayerID] = true
			switch item.State {
			case "confirmed":
				if !item.Before.valid() || !item.After.valid() || math.Hypot(item.Point.X-item.After.X, item.Point.Z-item.After.Z) > .5 {
					return nil
				}
				verified++
				attempted++
			case "uncertain":
				if !item.Before.valid() {
					return nil
				}
				attempted++
			case "rejected":
			case "prepared":
				if r.State != "preview" || !item.Before.valid() {
					return nil
				}
			default:
				return nil
			}
		}
		if r.State == "preview" {
			if _, err := uuid.Parse(r.PreviewID); err != nil || r.ExpiresAt <= r.ObservedAt || r.ExpiresAt-r.ObservedAt > 60 || r.Attempted != 0 || r.Verified != 0 {
				return nil
			}
			for _, item := range r.Items {
				if item.State != "prepared" {
					return nil
				}
			}
		} else if r.State == "cancelled" {
			if len(r.Items) != 0 || r.Attempted != 0 || r.Verified != 0 {
				return nil
			}
		} else if verified != r.Verified || attempted != r.Attempted || (r.State != "confirmed" && r.State != "partial") || (r.State == "confirmed") != (verified == len(r.Items)) {
			return nil
		}
		result = &r
	}
	return result
}
