package aicontrol

import "encoding/json"

// These examples use public identifiers only. They are shared by typed tools,
// compact discovery and validation feedback; no credential or user value is echoed.
func activityArgumentHint(operation string) string {
	switch operation {
	case "getRoomPlayerActivitySettings", "getRoomPlayerActivityStatus":
		return `Use {"path":{"roomId":"<id from listRooms>"}}. path is an object of API identifiers, not a filesystem path. Read recording, synchronization and historical coverage independently.`
	case "resyncRoomPlayerActivity":
		return `Example: {"path":{"roomId":"<id from listRooms>"},"requestId":"activity-resync-unique-key"}. Coalesces a wake-up; acceptance is not recovery. Read getRoomPlayerActivityStatus afterwards. Reuse requestId only for a retry of this same request.`
	case "updateRoomPlayerActivitySettings":
		return `First read getRoomPlayerActivitySettings for revision. Example: {"path":{"roomId":"<id from listRooms>"},"body":{"revision":"<latest revision>","positionIntervalSeconds":30},"requestId":"activity-config-unique-key"}. Preserve unspecified fields; appliedRevision acknowledges node application.`
	case "listRoomPlayerActivity":
		return `Example: {"path":{"roomId":"<id from listRooms>"},"query":{"userId":"KU_EXAMPLE","limit":10}}. Optional query: worldId from listRoomWorlds, from/to in RFC3339, kind, before. Filters go in query, never path. Use hasMore/nextCursor; inspect coverage, and do not treat missing records as inactivity.`
	case "getRoomPlayerTrajectory":
		return `userId is REQUIRED. Example: {"path":{"roomId":"<id from listRooms>"},"query":{"userId":"KU_EXAMPLE","projection":"compact","limit":10}}. Optional query: worldId from listRoomWorlds, from/to in RFC3339, before. Compact returns coordinate points only. Connect points only within the same nonempty continuousRangeId, world, producer and session; legacy/unknown continuity is points only. Inspect coverage and hasMore/nextCursor.`
	case "getRoomPlayerCombat":
		return `Example: {"path":{"roomId":"<id from listRooms>"},"query":{"userId":"KU_EXAMPLE"}}. Optional query: worldId from listRoomWorlds, from/to in RFC3339. Query filters never go in path. Check truncated; unresolved damage is unknown, not zero.`
	}
	return ""
}

func activityArgumentProblem(raw json.RawMessage, operation string) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return "arguments must be an object"
	}
	path, ok := in["path"].(map[string]any)
	if !ok {
		return "path must be an object containing roomId"
	}
	if v, ok := path["roomId"].(string); !ok || v == "" {
		return "path.roomId must be a nonempty string returned by listRooms"
	}
	if query, exists := in["query"]; exists {
		if _, ok := query.(map[string]any); !ok {
			return "query must be an object containing filters"
		}
	}
	if operation == "getRoomPlayerTrajectory" {
		q, _ := in["query"].(map[string]any)
		if v, ok := q["userId"].(string); !ok || v == "" {
			return "query.userId must be a nonempty player KU ID string"
		}
	}
	if operation == "updateRoomPlayerActivitySettings" {
		body, ok := in["body"].(map[string]any)
		if !ok {
			return "body must be an object containing the latest revision"
		}
		if _, ok := body["revision"].(string); !ok {
			return "body.revision must be a string from getRoomPlayerActivitySettings"
		}
	}
	if operation == "updateRoomPlayerActivitySettings" || operation == "resyncRoomPlayerActivity" {
		if _, ok := in["requestId"].(string); !ok {
			return "requestId must be a stable idempotency string of 8 to 128 characters"
		}
	}
	return "arguments do not match inputSchema; use only declared path/query/body/requestId fields"
}
