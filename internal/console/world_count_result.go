package console

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A successful transport without the fixed, request-bound result is not a count.
func describeWorldCount(run Run) Run {
	if run.Mode != "builtin" || run.CommandID != "count_world_entities" || run.Status != RunSucceeded {
		return run
	}
	fail := func() Run {
		run.Status = RunUncertain
		run.Message = "没有取得完整数量结果；不能解释为0，请查看原回执。"
		return run
	}
	if run.ExecutionOutcome != "confirmed" || run.Output == nil || run.Output.Truncated {
		return fail()
	}
	type countResult struct {
		RequestID        string `json:"requestId"`
		Prefab           string `json:"prefab"`
		Entities         *int   `json:"entities"`
		Quantity         *int64 `json:"quantity"`
		IncludeInventory *bool  `json:"includeInventory"`
		Complete         bool   `json:"complete"`
	}
	var result *countResult
	for _, line := range strings.Split(run.Output.Text, "\n") {
		const prefix = "[DST-ADMIN-COUNT-V1] "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if result != nil {
			return fail()
		}
		value := new(countResult)
		dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, prefix)))
		dec.DisallowUnknownFields()
		if dec.Decode(value) != nil || dec.Decode(new(any)) != io.EOF {
			return fail()
		}
		result = value
	}
	prefab, _ := run.Arguments["prefab"].(string)
	held, _ := run.Arguments["include_inventory"].(bool)
	if result == nil || !result.Complete || result.RequestID != run.ID || result.Prefab != prefab || result.Entities == nil || result.Quantity == nil || result.IncludeInventory == nil || *result.IncludeInventory != held || *result.Entities < 0 || *result.Entities > 50000 || *result.Quantity < int64(*result.Entities) || *result.Quantity > 5000000000 {
		return fail()
	}
	run.Message = fmt.Sprintf("%s：%d 个，共 %d 个实体。", prefab, *result.Quantity, *result.Entities)
	return run
}
