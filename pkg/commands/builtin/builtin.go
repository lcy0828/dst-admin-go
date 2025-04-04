package builtin

import (
	"dont/pkg/commands/types"
)

// GetBuiltinCommands 返回所有内置命令
func GetBuiltinCommands() []*types.Command {
	var builtinCommands []*types.Command

	// 添加所有内置命令
	builtinCommands = append(builtinCommands, getBasicCommands()...)
	builtinCommands = append(builtinCommands, getPlayerCommands()...)
	builtinCommands = append(builtinCommands, getWorldCommands()...)

	return builtinCommands
}

// getBasicCommands 返回基本操作的内置命令
func getBasicCommands() []*types.Command {
	return []*types.Command{
		{
			ID:          "save_world",
			Name:        "保存世界",
			Description: "保存当前游戏世界",
			Category:    "基础操作",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Save World]',c_save())",
			NeedsParams: false,
		},
		{
			ID:          "send_announcement",
			Name:        "发送公告",
			Description: "向所有玩家发送一条公告",
			Category:    "基础操作",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Send Announcement]',c_announce(\"{1}\"))",
			NeedsParams: true,
			ParamDesc:   "公告内容",
			Example:     "欢迎来到服务器",
		},
		{
			ID:          "regenerate_world",
			Name:        "重新生成世界",
			Description: "重新生成游戏世界",
			Category:    "基础操作",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Regenerate World]',c_regenerateworld())",
			NeedsParams: false,
		},
		{
			ID:          "shutdown_server",
			Name:        "关闭服务器",
			Description: "安全地关闭服务器",
			Category:    "基础操作",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Shutdown Server]',c_shutdown())",
			NeedsParams: false,
		},
		{
			ID:          "rollback",
			Name:        "回档",
			Description: "将世界回退指定天数",
			Category:    "基础操作",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Rollback]',c_rollback({1}))",
			NeedsParams: true,
			ParamDesc:   "回退的天数，例如：1",
			Example:     "1",
		},
	}
}

// getPlayerCommands 返回玩家相关的内置命令
func getPlayerCommands() []*types.Command {
	return []*types.Command{
		{
			ID:          "list_players",
			Name:        "玩家列表",
			Description: "获取当前世界中的玩家列表",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "for i, v in ipairs(TheNet:GetClientTable()) do print(string.format(\"[DST-ADMIN-GO] [Listplayers] [%d] [%s] [%s] [%s] [%s] \", i-1, string.format('%03d', v.playerage), v.userid, v.name, v.prefab)) end\n",
			NeedsParams: false,
		},
		{
			ID:          "kick_player",
			Name:        "踢出玩家",
			Description: "将指定玩家踢出服务器",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Kick Player]',TheNet:Kick(\"{1}\"))",
			NeedsParams: true,
			ParamDesc:   "玩家的KU ID",
			Example:     "KU_xxxxxxxx",
		},
		{
			ID:          "ban_player",
			Name:        "封禁玩家",
			Description: "封禁指定玩家",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Ban Player]',TheNet:Ban(\"{1}\"))",
			NeedsParams: true,
			ParamDesc:   "玩家的KU ID",
			Example:     "KU_xxxxxxxx",
		},
		{
			ID:          "unban_player",
			Name:        "解封玩家",
			Description: "解除对指定玩家的封禁",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Unban Player]',TheNet:Unban(\"{1}\"))",
			NeedsParams: true,
			ParamDesc:   "玩家的KU ID",
			Example:     "KU_xxxxxxxx",
		},
		{
			ID:          "give_item",
			Name:        "给予物品",
			Description: "给指定玩家一个物品",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Give Item]',c_select(\"{1}\"):Give(\"{2}\", {3}))",
			NeedsParams: true,
			ParamDesc:   "玩家名称，物品ID，数量",
			Example:     "玩家名 flint 10",
		},
	}
}

// getWorldCommands 返回世界相关的内置命令
func getWorldCommands() []*types.Command {
	return []*types.Command{
		{
			ID:          "world_basic_info",
			Name:        "世界基本信息",
			Description: "获取当前世界的基本信息",
			Category:    "世界信息",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[World Basic Info]', TheWorld.state.cycles + 1,TheWorld.state.season,TheWorld.state.remainingdaysinseason)",
			NeedsParams: false,
		},
		{
			ID:          "set_season",
			Name:        "设置季节",
			Description: "设置当前世界的季节",
			Category:    "世界控制",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Set Season]',TheWorld:PushEvent(\"ms_setseason\", \"{1}\"))",
			NeedsParams: true,
			ParamDesc:   "季节名称 (autumn, winter, spring, summer)",
			Example:     "summer",
		},
		{
			ID:          "set_day",
			Name:        "设置天数",
			Description: "设置当前世界的天数",
			Category:    "世界控制",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Set Day]',TheWorld:PushEvent(\"ms_setclocksegs\", {day={1}, dusk=2, night=2}))",
			NeedsParams: true,
			ParamDesc:   "白天的时长(段数)",
			Example:     "12",
		},
		{
			ID:          "spawn_entity",
			Name:        "生成实体",
			Description: "在指定位置生成实体",
			Category:    "世界控制",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[Spawn Entity]',c_spawn(\"{1}\", {2}, {3}, {4}))",
			NeedsParams: true,
			ParamDesc:   "实体ID, x坐标, y坐标, z坐标",
			Example:     "pigman 0 0 0",
		},
		{
			ID:          "godmode_player",
			Name:        "玩家无敌模式",
			Description: "开启或关闭指定玩家的无敌模式",
			Category:    "玩家管理",
			IsBuiltin:   true,
			Script:      "print('[DST-ADMIN-GO]','[God Mode]', c_select(\"{1}\").components.health:SetInvincible({2}))",
			NeedsParams: true,
			ParamDesc:   "玩家名称，true或false",
			Example:     "玩家名 true",
		},
	}
}
