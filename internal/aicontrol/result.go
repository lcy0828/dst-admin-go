package aicontrol

import (
	"bytes"
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// Full schemas are explicitly requested for a single operation and already
// bounded by discovery. Never cut a JSON Schema into an unusable excerpt.
// Actual game/log/operation results retain the smaller normal context budget.
func modelToolResult(data json.RawMessage, tool string, arguments json.RawMessage, status string) json.RawMessage {
	budget := 8 << 10
	if tool == discoverAPI && status == "succeeded" {
		var options struct {
			IncludeSchema       bool `json:"includeSchema"`
			IncludeResultSchema bool `json:"includeResultSchema"`
		}
		if json.Unmarshal(arguments, &options) == nil && (options.IncludeSchema || options.IncludeResultSchema) {
			budget = 64 << 10
		}
	}
	return boundResult(data, budget)
}

// boundResult retains structured evidence and receipt identifiers. A large
// output must never turn a successful operation into a failure or invite a retry.
// It also supplies useful excerpts to the model within its smaller context budget.
func boundResult(raw json.RawMessage, budget int) json.RawMessage {
	if len(raw) <= budget {
		return raw
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return json.RawMessage(`{"error":"AI_INVALID_RESULT"}`)
	}
	for size, entries := budget/4, 64; size >= 64; size, entries = size/2, max(1, entries/2) {
		bounded := excerpt(value, size, entries, 0)
		if object, ok := bounded.(map[string]any); ok {
			object["_aiTruncated"] = true
		} else {
			bounded = map[string]any{"data": bounded, "_aiTruncated": true}
		}
		encoded, _ := json.Marshal(bounded)
		if len(encoded) <= budget {
			return encoded
		}
	}
	return json.RawMessage(`{"_aiTruncated":true}`)
}

func excerpt(value any, size, entries, depth int) any {
	if depth >= 16 {
		return "[truncated]"
	}
	switch x := value.(type) {
	case string:
		if len(x) <= size {
			return x
		}
		// Keep both the beginning and the latest output, preserving UTF-8.
		end, start := size/2, len(x)-size/2
		for end > 0 && !utf8.RuneStart(x[end]) {
			end--
		}
		for start < len(x) && !utf8.RuneStart(x[start]) {
			start++
		}
		return x[:end] + "\n[truncated]\n" + x[start:]
	case []any:
		result := make([]any, 0, min(len(x), entries))
		for i := 0; i < min(len(x), entries); i++ {
			index := i
			if len(x) > entries && i >= (entries+1)/2 {
				index = len(x) - entries + i
			}
			result = append(result, excerpt(x[index], size, entries, depth+1))
		}
		return result
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left, right := receiptField(keys[i]), receiptField(keys[j])
			if left != right {
				return left
			}
			return keys[i] < keys[j]
		})
		result := make(map[string]any)
		for i, key := range keys {
			if i >= entries && !receiptField(key) {
				continue
			}
			if text, ok := x[key].(string); ok && identityField(key) && len(text) <= 255 {
				result[key] = text
			} else {
				result[key] = excerpt(x[key], size, entries, depth+1)
			}
		}
		return result
	default:
		return value
	}
}

func identityField(key string) bool {
	switch key {
	case "id", "callId", "jobId", "runId", "roomId", "worldId", "url", "operationId":
		return true
	}
	return false
}

func receiptField(key string) bool {
	switch key {
	case "id", "callId", "jobId", "runId", "roomId", "worldId", "status", "state",
		"error", "errorCode", "errorMessage", "code", "message", "data", "result",
		"mayHaveExecuted", "executionOutcome", "transportOutcome", "createdAt", "finishedAt":
		return true
	case "resource", "privateResult", "url", "httpStatus", "operationId":
		return true
	}
	return false
}
