package aicontrol

import (
	"dont/internal/apicontract"
	consoleapi "dont/internal/console"
)

func workbenchCapabilities() []BotCapability {
	out := []BotCapability{}
	for _, definition := range consoleapi.BuiltinWorkbenchDefinitions() {
		d := definition
		props := map[string]any{}
		required := []string{}
		hints := map[string]string{}
		hasPlayer := false
		for _, p := range d.Parameters {
			schema := map[string]any{"type": p.Type, "title": p.Label, "description": p.Description}
			switch p.Type {
			case "string":
				schema["minLength"] = 1
				schema["maxLength"] = 500
			case "enum":
				schema["type"] = "string"
				schema["enum"] = p.Options
			}
			if p.Minimum != nil {
				schema["minimum"] = *p.Minimum
			}
			if p.Maximum != nil {
				schema["maximum"] = *p.Maximum
			}
			if p.Default != nil {
				schema["default"] = p.Default
			}
			props[p.Name] = schema
			if p.Required {
				required = append(required, p.Name)
			}
			if p.Name == "player_id" || p.Name == "other_player_id" || p.Name == "destination_id" {
				hints[p.Name] = "player"
			}
			if p.Name == "player_id" && p.Required {
				hasPlayer = true
			}
		}
		props["worldId"] = botString(255)
		hints["worldId"] = "world"
		if !hasPlayer {
			required = append(required, "worldId")
		}
		if d.Risk == consoleapi.RiskCritical {
			props["confirmed"] = map[string]any{"type": "boolean", "enum": []bool{true}, "description": "危险操作需要用户明确二次确认"}
			required = append(required, "confirmed")
		}
		id := "workbench." + d.ID
		if d.ID == "resurrect_player" {
			id = "player.resurrect"
		}
		out = append(out, BotCapability{ID: id, Title: d.Name, Category: "management", Access: "write", Risk: string(d.Risk), Operation: "executeBuiltinRoomCommand", Parameters: botSchema(props, required...), ParameterHints: hints, build: func(room string, p map[string]any) apicontract.Input {
			args := map[string]any{}
			for _, parameter := range d.Parameters {
				if v, ok := p[parameter.Name]; ok {
					args[parameter.Name] = v
				}
			}
			body := map[string]any{"commandId": d.ID, "arguments": args}
			if world, ok := p["worldId"]; ok {
				body["worldId"] = world
			}
			if v, ok := p["confirmed"]; ok {
				body["confirmed"] = v
			}
			return apicontract.Input{Path: map[string]any{"roomId": room}, Body: body}
		}})
	}
	return out
}
