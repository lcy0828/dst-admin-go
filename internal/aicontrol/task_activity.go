package aicontrol

import "dont/internal/apicontract"

func activityCapabilities() []BotCapability {
	list := []BotCapability{}
	for _, v := range []struct{ id, title, operation string }{
		{"activity", "玩家活动", "listRoomPlayerActivity"},
		{"trajectory", "玩家移动轨迹", "getRoomPlayerTrajectory"},
		{"combat", "玩家战斗记录", "getRoomPlayerCombat"},
	} {
		v := v
		props := map[string]any{"playerId": botString(128), "from": map[string]any{"type": "string", "format": "date-time", "maxLength": 64}, "to": map[string]any{"type": "string", "format": "date-time", "maxLength": 64}}
		if v.id != "combat" {
			props["before"] = botString(1024)
			props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 50}
		}
		list = append(list, BotCapability{ID: "player." + v.id, Title: v.title, Category: "profile", Operation: v.operation, Parameters: botSchema(props, "playerId"), ParameterHints: map[string]string{"playerId": "player"}, build: func(room string, p map[string]any) apicontract.Input {
			query := map[string]any{"userId": p["playerId"]}
			for _, k := range []string{"from", "to", "before", "limit"} {
				if value, ok := p[k]; ok {
					query[k] = value
				}
			}
			if v.id != "combat" {
				if _, ok := query["limit"]; !ok {
					query["limit"] = 10
				}
			}
			if v.id == "trajectory" {
				query["projection"] = "compact"
			}
			return apicontract.Input{Path: map[string]any{"roomId": room}, Query: query}
		}})
	}
	return list
}
