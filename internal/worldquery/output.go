package worldquery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"dont/internal/dstruntime"
)

func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
func problem(r Result, status, code, message string) Result {
	r.Status = status
	r.Complete = false
	r.Data = nil
	r.Error = &QueryError{code, message}
	return r
}
func acceptReceipt(r Result, q Request, receipt dstruntime.CommandReceipt) Result {
	r.TransportOutcome = "sent"
	r.ExecutionOutcome = "confirmed"
	r.MayHaveExecuted = true
	r.RuntimeCode = receipt.Code
	at := receipt.CompletedAt
	r.ObservedAt = &at
	if !receipt.OK {
		r.ExecutionOutcome = "rejected"
		r.ResultState = "failed"
		return problem(r, "failed", "QUERY_EXECUTION_FAILED", "Runtime rejected the fixed query")
	}
	r.ResultState = "output_unavailable"
	output, ok := receipt.Details["output"].(map[string]interface{})
	if !ok {
		return problem(r, "unavailable", "QUERY_OUTPUT_UNAVAILABLE", "Runtime did not capture output")
	}
	text, ok := output["text"].(string)
	truncated, hasFlag := output["truncated"].(bool)
	if !ok || !hasFlag || truncated || len(text) > 8193 || !utf8.ValidString(text) {
		return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Output is missing, truncated or invalid")
	}
	if strings.TrimSpace(text) == "" {
		r.ResultState = "no_output"
		return problem(r, "unavailable", "QUERY_NO_OUTPUT", "No synchronous query output was returned; this is not a zero result")
	}
	var wire struct {
		ProtocolVersion int             `json:"protocolVersion"`
		RunID           string          `json:"runId"`
		QueryID         string          `json:"queryId"`
		Complete        *bool           `json:"complete"`
		Data            json.RawMessage `json:"data"`
		Reason          string          `json:"reason"`
		Scanned         *int            `json:"scannedEntities"`
	}
	if strictJSON([]byte(text), &wire) != nil || wire.ProtocolVersion != 1 || wire.RunID != r.QueryRunID || wire.QueryID != q.QueryID || wire.Complete == nil {
		return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Output does not match the original query")
	}
	complete := *wire.Complete
	if len(bytes.TrimSpace(wire.Data)) == 0 || bytes.TrimSpace(wire.Data)[0] != '{' {
		return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Missing query data")
	}
	if complete && wire.Reason != "" || !complete && wire.Reason != "SCAN_LIMIT" && wire.Reason != "CPU_BUDGET" && wire.Reason != "OUTPUT_LIMIT" {
		return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid completion evidence")
	}
	var value any
	switch q.QueryID {
	case "entity_count":
		var data entityData
		if strictJSON(wire.Data, &data) != nil || data.Prefab != q.Prefab || wire.Scanned == nil || *wire.Scanned < 0 || *wire.Scanned > 100000 {
			return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid entity scan result")
		}
		if complete {
			if data.Count == nil || *data.Count < 0 || *data.Count > *wire.Scanned || data.LowerBound != nil {
				return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Missing complete entity count")
			}
		} else {
			if data.Count != nil || data.LowerBound == nil || *data.LowerBound < 0 || *data.LowerBound > *wire.Scanned {
				return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid partial entity count")
			}
		}
		r.ScannedEntities = wire.Scanned
		r.ScanLimit = 100000
		value = data
	case "world_info":
		var data worldData
		if !complete || strictJSON(wire.Data, &data) != nil || (data.Season != nil && len(*data.Season) > 64) || (data.Phase != nil && len(*data.Phase) > 64) {
			return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid world state result")
		}
		value = data
	case "online_players":
		var data struct {
			Players json.RawMessage `json:"players"`
			Count   *int            `json:"count"`
		}
		if strictJSON(wire.Data, &data) != nil || data.Count == nil || *data.Count < 0 || *data.Count > 64 {
			return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid player result")
		}
		items := []player{}
		if bytes.Equal(bytes.TrimSpace(data.Players), []byte("null")) {
			return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Missing player list")
		}
		if string(data.Players) != "{}" || *data.Count != 0 {
			if strictJSON(data.Players, &items) != nil {
				return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid player list")
			}
		}
		if len(items) != *data.Count {
			return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Missing player list")
		}
		seen := map[string]bool{}
		for _, p := range items {
			if p.ID == "" || len(p.ID) > 80 || len(p.Name) > 192 || len(p.Prefab) > 80 || seen[p.ID] {
				return problem(r, "unavailable", "QUERY_OUTPUT_INVALID", "Invalid player projection")
			}
			seen[p.ID] = true
		}
		value = playerData{items, *data.Count}
	}
	r.Data, _ = json.Marshal(value)
	r.Complete = complete
	r.Status = "succeeded"
	r.ResultState = "output"
	r.Error = nil
	if !complete {
		r.Status = "partial"
		r.Error = &QueryError{wire.Reason, "Query budget reached; returned data is incomplete"}
	}
	return r
}
