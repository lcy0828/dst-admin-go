package aicontrol

import (
	"dont/internal/apicontract"
	"dont/internal/console"
	"dont/internal/landmarks"
)

func locationCapabilities() []BotCapability {
	actions := []console.Action{}
	for _, destination := range landmarks.Destinations() {
		terms := append([]string{destination.Name, destination.ID}, destination.Aliases...)
		actions = append(actions, console.Action{ID: destination.ID, Title: "传送到" + destination.Name, SearchTerms: terms, Parameters: map[string]string{"locationName": destination.Name}})
	}
	locationName := botString(48)
	locationName["description"] = "可填写自己保存的地点名，也可直接填写猪王、出生点、月岛、绿洲、龙蝇沙漠、蜂后。同名时优先使用已保存地点。仅当前分片，有多个候选时先选择。"
	body := func(room string, p map[string]any) apicontract.Input {
		return apicontract.Input{Path: map[string]any{"roomId": room}, Body: p}
	}
	teleportBody := func(room string, p map[string]any) apicontract.Input {
		values := make(map[string]any, len(p))
		for k, v := range p {
			values[k] = v
		}
		if id, _ := values["locationId"].(string); id != "" {
			// A selected saved record stays exact even if its name changes.
			delete(values, "locationName")
		}
		return body(room, values)
	}
	return []BotCapability{
		// Saved destinations are room information; reading them does not run a
		// live world query and should share the ordinary room overview category.
		{ID: "room.locations", Title: "已保存地点", Category: "status", Operation: "listRoomLocations", Parameters: botSchema(map[string]any{"worldId": botString(255), "search": botString(48), "offset": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}}), build: func(room string, p map[string]any) apicontract.Input {
			return apicontract.Input{Path: map[string]any{"roomId": room}, Query: p}
		}},
		{ID: "location.save", Title: "保存玩家当前位置", Category: "management", Access: "write", Risk: "high", Operation: "createRoomLocation", Parameters: botSchema(map[string]any{"playerId": botString(128), "name": botString(48)}, "playerId", "name"), ParameterHints: map[string]string{"playerId": "player"}, build: body},
		{ID: "player.teleport", Title: "传送玩家到地点", Category: "management", Access: "write", Risk: "high", Operation: "prepareRoomTeleport", Actions: actions, Parameters: botSchema(map[string]any{"playerId": botString(128), "locationName": locationName, "locationId": botString(36), "worldId": botString(255), "landmarkRef": botString(100)}, "playerId", "locationName"), ParameterHints: map[string]string{"playerId": "player"}, build: teleportBody},
	}
}
