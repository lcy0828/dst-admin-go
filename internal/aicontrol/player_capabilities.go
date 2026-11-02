package aicontrol

import "dont/internal/apicontract"

// Keep the legacy player.resurrect workbench contract intact. These recipes
// expose the other fixed player endpoints without accepting scripts or routes.
func playerCapabilities() []BotCapability {
	var out []BotCapability
	for _, action := range []struct {
		id, title, operation, risk string
	}{
		{"god_mode", "无敌模式（上帝模式）", "setRoomPlayerGodMode", "high"},
		{"creative_mode", "制作模式（创造模式）", "setRoomPlayerCreativeMode", "high"},
		{"announce", "提醒玩家（所在世界广播）", "announceToRoomPlayer", "medium"},
		{"ban", "封禁玩家", "banRoomPlayer", "high"},
		{"unban", "解除玩家封禁", "unbanRoomPlayer", "high"},
		{"kill", "使玩家死亡", "killRoomPlayer", "critical"},
		{"reset_character", "重选角色（重置玩家）", "resetRoomPlayerCharacter", "critical"},
	} {
		props := map[string]any{"playerId": botString(128), "worldId": botString(255)}
		required := []string{"playerId"}
		bodyFields := []string{"worldId"}
		switch action.id {
		case "god_mode", "creative_mode":
			props["enabled"] = map[string]any{"type": "boolean", "description": "true 明确开启，false 明确关闭；不能省略或用切换代替。"}
			required = append(required, "enabled")
			bodyFields = append(bodyFields, "enabled")
		case "announce":
			props["message"] = botString(300)
			required = append(required, "message")
			bodyFields = append(bodyFields, "message")
		case "ban", "unban":
			props["confirmation"] = botString(255)
			props["confirmation"].(map[string]any)["description"] = "填写完整房间名称以确认修改封禁名单。"
			required = append(required, "confirmation")
			bodyFields = append(bodyFields, "confirmation")
			if action.id == "ban" {
				props["reason"] = botString(300)
				props["duration"] = map[string]any{"type": "string", "enum": []string{"1h", "6h", "12h", "1d", "3d", "7d", "30d", "permanent"}}
				required = append(required, "reason", "duration")
				bodyFields = append(bodyFields, "reason", "duration")
			}
		case "kill", "reset_character":
			props["confirmed"] = map[string]any{"type": "boolean", "enum": []bool{true}, "description": "影响玩家角色或进度；用户明确确认后才可提交。"}
			required = append(required, "confirmed")
		}
		out = append(out, BotCapability{
			ID: "player." + action.id, Title: action.title, Category: "management", Access: "write", Risk: action.risk,
			Operation: action.operation, Parameters: botSchema(props, required...),
			ParameterHints: map[string]string{"playerId": "player", "worldId": "world"},
			build: func(room string, p map[string]any) apicontract.Input {
				body := map[string]any{}
				for _, field := range bodyFields {
					if value, ok := p[field]; ok {
						body[field] = value
					}
				}
				if action.id == "kill" || action.id == "reset_character" {
					body["confirmation"] = p["playerId"]
				}
				return apicontract.Input{Path: map[string]any{"roomId": room, "playerId": p["playerId"]}, Body: body}
			},
		})
	}
	return out
}
