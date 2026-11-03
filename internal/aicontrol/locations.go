package aicontrol

import "dont/internal/apicontract"

func locationCapabilities() []BotCapability {
	body := func(room string, p map[string]any) apicontract.Input {
		return apicontract.Input{Path: map[string]any{"roomId": room}, Body: p}
	}
	return []BotCapability{
		// Saved destinations are room information; reading them does not run a
		// live world query and should share the ordinary room overview category.
		{ID: "room.locations", Title: "已保存地点", Category: "status", Operation: "listRoomLocations", Parameters: botSchema(map[string]any{"worldId": botString(255), "search": botString(48), "offset": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}}), build: func(room string, p map[string]any) apicontract.Input {
			return apicontract.Input{Path: map[string]any{"roomId": room}, Query: p}
		}},
		{ID: "location.save", Title: "保存玩家当前位置", Category: "management", Access: "write", Risk: "high", Operation: "createRoomLocation", Parameters: botSchema(map[string]any{"playerId": botString(128), "name": botString(48)}, "playerId", "name"), ParameterHints: map[string]string{"playerId": "player"}, build: body},
		{ID: "player.teleport", Title: "传送到已保存地点", Category: "management", Access: "write", Risk: "high", Operation: "prepareRoomTeleport", Parameters: botSchema(map[string]any{"playerId": botString(128), "locationName": botString(48)}, "playerId", "locationName"), ParameterHints: map[string]string{"playerId": "player"}, build: body},
	}
}
