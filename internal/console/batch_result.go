package console

import (
	"bytes"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"strings"
)

type BatchResult struct {
	State       string   `json:"state"`
	PreviewID   string   `json:"previewId,omitempty"`
	ExpiresIn   int      `json:"expiresIn,omitempty"`
	Matched     int      `json:"matched"`
	Attempted   int      `json:"attempted"`
	Verified    int      `json:"verified"`
	Unconfirmed int      `json:"unconfirmed"`
	Skipped     int      `json:"skipped"`
	Failed      int      `json:"failed"`
	Reason      string   `json:"reason,omitempty"`
	X           *float64 `json:"x,omitempty"`
	Z           *float64 `json:"z,omitempty"`
}

func batchResult(run Run) *BatchResult {
	if run.Status != RunSucceeded || run.ExecutionOutcome != "confirmed" || run.Mode != "builtin" || run.CommandID != "act_nearby_entities" || run.Output == nil || run.Output.Truncated || !strings.Contains(run.Script, "w.Nearby(") {
		return nil
	}
	var result *BatchResult
	for _, line := range strings.Split(run.Output.Text, "\n") {
		const prefix = "[DST-ADMIN-BATCH-V1] "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if result != nil || len(line) > 2048 {
			return nil
		}
		text := []byte(strings.TrimPrefix(line, prefix))
		var fields map[string]json.RawMessage
		if json.Unmarshal(text, &fields) != nil {
			return nil
		}
		for _, key := range []string{"state", "matched", "attempted", "verified", "unconfirmed", "skipped", "failed"} {
			if len(fields[key]) == 0 || string(fields[key]) == "null" {
				return nil
			}
		}
		var r BatchResult
		d := json.NewDecoder(bytes.NewReader(text))
		d.DisallowUnknownFields()
		if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
			return nil
		}
		if r.Reason != "" && r.Reason != "CPU_BUDGET" {
			return nil
		}
		for _, n := range []int{r.Matched, r.Attempted, r.Verified, r.Unconfirmed, r.Skipped, r.Failed} {
			if n < 0 || n > 40 {
				return nil
			}
		}
		switch r.State {
		case "preview", "cancelled":
			if r.Attempted+r.Verified+r.Unconfirmed+r.Skipped+r.Failed != 0 {
				return nil
			}
			if r.State == "preview" {
				if _, e := uuid.Parse(r.PreviewID); e != nil || r.ExpiresIn != 30 || r.X == nil || r.Z == nil {
					return nil
				}
			}
		case "completed", "partial":
			if r.Verified+r.Unconfirmed+r.Skipped+r.Failed != r.Matched || r.Attempted > r.Matched || r.Verified+r.Unconfirmed > r.Attempted {
				return nil
			}
			if (r.State == "completed") != (r.Verified == r.Matched) {
				return nil
			}
		default:
			return nil
		}
		result = &r
	}
	return result
}
