package console

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"math"
	"strings"
)

// V1 is deliberately decoded with the original wire type. V2 never relaxes a
// historical receipt's validation, nor trusts arbitrary console print output.
func batchResultV2(run Run) *BatchResult {
	if run.Mode != "builtin" || run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated {
		return nil
	}
	action, _ := run.Arguments["action"].(string)
	expected := action
	if run.CommandID == "act_nearby_entities" {
		if !nearbyV2(action) || !strings.Contains(run.Script, "w.NearbyV2(") {
			return nil
		}
	} else {
		expected = run.CommandID
		if actionInteraction(run.CommandID, "") == nil || !strings.Contains(run.Script, "w.Setting(") {
			return nil
		}
	}
	var out *BatchResult
	for _, line := range strings.Split(run.Output.Text, "\n") {
		if strings.HasPrefix(line, "[DST-ADMIN-BATCH-V1] ") {
			return nil
		}
		const prefix = "[DST-ADMIN-BATCH-V2] "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if out != nil || len(line) > 12*1024 {
			return nil
		}
		raw := []byte(strings.TrimPrefix(line, prefix))
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		for _, key := range []string{"version", "action", "requestId", "worldId", "state", "matched", "attempted", "verified", "unconfirmed", "skipped", "failed", "units", "items", "detailsComplete"} {
			if len(fields[key]) == 0 || string(fields[key]) == "null" {
				return nil
			}
		}
		var r BatchResult
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
			return nil
		}
		if r.Version != 2 || r.Action != expected || r.RequestID != run.ID || len(r.WorldID) < 1 || len(r.WorldID) > 80 || r.Units < 0 || r.Units > 200 || !r.DetailsComplete || r.Reason != "" {
			return nil
		}
		if run.CommandID != "act_nearby_entities" && r.Matched != 1 {
			return nil
		}
		if _, e := uuid.Parse(r.RequestID); e != nil {
			return nil
		}
		for _, n := range []int{r.Matched, r.Attempted, r.Verified, r.Unconfirmed, r.Skipped, r.Failed} {
			if n < 0 || n > 40 {
				return nil
			}
		}
		if r.X != nil && (math.IsNaN(*r.X) || math.IsInf(*r.X, 0)) || r.Z != nil && (math.IsNaN(*r.Z) || math.IsInf(*r.Z, 0)) {
			return nil
		}
		counts := map[string]int{}
		units := 0
		for _, item := range r.Items {
			if !prefabPattern.MatchString(item.Prefab) || item.Units < 0 || item.Units > 200 {
				return nil
			}
			for _, v := range []*float64{item.Before, item.After} {
				if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || math.Abs(*v) > 1e9) {
					return nil
				}
			}
			if item.Destination != "none" && item.Destination != "inventory" && item.Destination != "rebuild" {
				return nil
			}
			switch item.Reason {
			case "", "CPU_BUDGET", "TARGET_CHANGED", "TARGET_UNAVAILABLE", "INVENTORY_FULL", "INVENTORY_UNAVAILABLE", "MATERIAL_UNAVAILABLE", "REBUILD_UNAVAILABLE", "REBUILD_UNCONFIRMED", "EFFECT_UNCONFIRMED", "OUTPUT_UNCONFIRMED", "TRANSITION_PENDING":
			default:
				return nil
			}
			switch item.State {
			case "prepared", "verified", "unconfirmed", "skipped", "failed":
			default:
				return nil
			}
			if (item.State == "verified" || item.State == "prepared") && item.Reason != "" {
				return nil
			}
			if item.State != "verified" && item.State != "prepared" && item.Units != 0 {
				return nil
			}
			counts[item.State]++
			units += item.Units
		}
		switch r.State {
		case "preview":
			if _, err := uuid.Parse(r.PreviewID); err != nil || r.ExpiresIn != 30 || r.ExpiresAt-r.ObservedAt != 30 || r.ObservedAt <= 0 || r.X == nil || r.Z == nil {
				return nil
			}
			if r.Attempted+r.Verified+r.Unconfirmed+r.Skipped+r.Failed+r.Units != 0 || counts["prepared"] != r.Matched || len(r.Items) != r.Matched || units > 200 {
				return nil
			}
		case "cancelled":
			if r.Attempted+r.Verified+r.Unconfirmed+r.Skipped+r.Failed+r.Units != 0 || len(r.Items) != 0 {
				return nil
			}
		case "completed", "partial":
			if r.Matched != r.Verified+r.Unconfirmed+r.Skipped+r.Failed || r.Attempted > r.Matched || r.Verified+r.Unconfirmed > r.Attempted || (r.State == "completed") != (r.Verified == r.Matched) {
				return nil
			}
			if len(r.Items) != r.Matched || counts["prepared"] != 0 || counts["verified"] != r.Verified || counts["unconfirmed"] != r.Unconfirmed || counts["skipped"] != r.Skipped || counts["failed"] != r.Failed || units != r.Units {
				return nil
			}
		default:
			return nil
		}
		out = &r
	}
	return out
}
