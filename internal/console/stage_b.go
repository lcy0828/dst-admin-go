package console

import (
	"fmt"
	"github.com/google/uuid"
)

func addStageBTemplates(templates map[string]template) {
	min, max := 0, 5
	definitions := []Definition{
		{ID: "set_player_hunger_rate", Name: "调整饥饿消耗", Description: "设置面板附加的饥饿消耗倍率，或解除面板设置；保留其他模组效果，离线、换世界或运行脚本更新后不重新施加。", Category: "玩家管理", Risk: RiskHigh, IsBuiltin: true,
			Parameters: []Parameter{{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true}, {Name: "mode", Label: "设置方式", Type: "enum", Required: true, Options: []string{"set", "remove"}}, {Name: "multiplier", Label: "饥饿消耗倍率", Type: "number", Minimum: &min, Maximum: &max, Default: 1}}},
		{ID: "set_wanda_age", Name: "调整旺达年龄", Description: "将在线旺达调整为青年、中年或老年；年龄和生命联动，不恢复物品冷却。", Category: "玩家管理", Risk: RiskHigh, IsBuiltin: true,
			Parameters: []Parameter{{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true}, {Name: "age", Label: "目标年龄段", Type: "enum", Required: true, Options: []string{"young", "normal", "old"}}}},
		{ID: "restore_woodie_human", Name: "伍迪恢复人形", Description: "使用原生变身流程恢复在线伍迪；需等待变身动画完成，未确认结果时不要重复执行。", Category: "玩家管理", Risk: RiskHigh, IsBuiltin: true,
			Parameters: []Parameter{{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true}}},
	}
	for _, def := range definitions {
		d := def
		d.Parameters = append(d.Parameters, Parameter{Name: "phase", Label: "操作步骤", Type: "enum", Options: []string{"preview", "execute", "cancel"}, Default: "preview"}, Parameter{Name: "preview_id", Label: "预览编号", Type: "string"})
		templates[d.ID] = template{definition: d, render: func(args map[string]interface{}) (string, error) {
			player, err := matchedArgument(args, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			value := ""
			amount := 0.0
			switch d.ID {
			case "set_player_hunger_rate":
				value, err = enumArgument(args, "mode", "set", "remove")
				if err == nil && value == "set" {
					amount, err = floatArgument(args, "multiplier", 0, 5)
				}
			case "set_wanda_age":
				value, err = enumArgument(args, "age", "young", "normal", "old")
			}
			if err != nil {
				return "", err
			}
			phase := nearbyPhase(args)
			if phase != "preview" && phase != "execute" && phase != "cancel" {
				return "", ErrInvalidArguments
			}
			token, _ := args["preview_id"].(string)
			if phase != "preview" {
				if _, err := uuid.Parse(token); err != nil {
					return "", ErrInvalidArguments
				}
			}
			return fmt.Sprintf(`local r=rawget(_G,"DSTAdmin");local w=r and r.Commands and r.Commands.Workbench;if not w or not w.Setting or w.ResultVersion~=2 then error("WORKBENCH_UPDATE_REQUIRED")end;w.Setting(%s,%s,%s,%s,%s,%s,%s,_dst_admin_batch_caller,_dst_admin_batch_request)`, quoteLua(player), quoteLua(d.ID), quoteLua(value), luaNumber(amount), quoteLua(phase), quoteLua(token), quoteLua(uuid.NewString())), nil
		}}
	}
}
