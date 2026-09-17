package console

import (
	"fmt"
	"math"
	"strconv"
)

func addGameToolTemplates(templates map[string]template) {
	minStat, maxStat := -20, 100
	minSpeed, maxSpeed := -2, 100
	minTimeScale, maxTimeScale := 0, 20
	minDays, maxDays := 1, 200
	minWorldTemperature, maxWorldTemperature := -25, 95
	minRadius, maxRadius := 1, 64
	minLockValue, maxLockValue := -20, 999
	minAbilityValue, maxAbilityValue := 0, 99999

	templates["set_player_stat"] = template{
		definition: Definition{
			ID: "set_player_stat", Name: "设置玩家状态", Description: "设置在线玩家的生命、饥饿、理智、湿度或温度", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "stat", Label: "状态", Type: "enum", Required: true, Options: []string{"health", "hunger", "sanity", "moisture", "temperature"}},
				{Name: "value", Label: "数值", Type: "number", Required: true, Minimum: &minStat, Maximum: &maxStat},
			},
			Script: `按 KU ID 查找玩家并设置指定状态组件`, IsBuiltin: true,
		},
		render: renderSetPlayerStat,
	}
	templates["set_player_speed"] = template{
		definition: Definition{
			ID: "set_player_speed", Name: "设置玩家移速", Description: "设置在线玩家的外部移动速度倍率", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "multiplier", Label: "移动速度倍率", Type: "number", Required: true, Minimum: &minSpeed, Maximum: &maxSpeed, Default: 1},
			},
			Script: `按 KU ID 查找玩家并设置受控外部移速倍率`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			multiplier, err := floatArgument(arguments, "multiplier", -2, 100)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.locomotor then p.components.locomotor:SetExternalSpeedMultiplier(p,"dst_admin_go",%s);print("[DST-ADMIN-PLAYER-SPEED]",%s,%s)else print("[DST-ADMIN-PLAYER-SPEED]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), luaNumber(multiplier), quoteLua(playerID), luaNumber(multiplier), quoteLua(playerID)), nil
		},
	}
	templates["set_player_lock"] = template{
		definition: Definition{
			ID: "set_player_lock", Name: "设置玩家状态锁", Description: "按 TMIR 语义开启或关闭生命下限、湿度或温度锁定", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "resource", Label: "锁定项目", Type: "enum", Required: true, Options: []string{"health", "moisture", "temperature"}},
				{Name: "value_mode", Label: "生命锁定单位", Type: "enum", Options: []string{"absolute", "percent"}, Default: "absolute", Description: "仅生命锁定支持百分比"},
				{Name: "enabled", Label: "启用", Type: "boolean", Required: true, Default: true},
				{Name: "value", Label: "锁定值", Type: "number", Required: true, Minimum: &minLockValue, Maximum: &maxLockValue},
			},
			Script: `按 KU ID 设置受控的玩家状态锁定任务`, IsBuiltin: true,
		},
		render: renderSetPlayerLock,
	}
	templates["adjust_player_luck"] = template{
		definition: Definition{
			ID: "adjust_player_luck", Name: "调整玩家幸运值", Description: "增加、减少或清除 TMIR 控制台幸运值来源", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "mode", Label: "调整方式", Type: "enum", Required: true, Options: []string{"add", "subtract", "clear"}},
			},
			Script: `通过 luckuser 的独立来源调整幸运值`, IsBuiltin: true,
		},
		render: renderAdjustPlayerLuck,
	}
	templates["manage_player_skills"] = template{
		definition: Definition{
			ID: "manage_player_skills", Name: "管理玩家洞察技能", Description: "授予大量技能经验或重置当前角色已激活技能", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "mode", Label: "操作", Type: "enum", Required: true, Options: []string{"grant_xp", "reset"}},
			},
			Script: `通过 skilltreeupdater 授予经验或逐项停用技能`, IsBuiltin: true,
		},
		render: renderManagePlayerSkills,
	}
	templates["set_monkey_curse"] = template{
		definition: Definition{
			ID: "set_monkey_curse", Name: "设置猴子诅咒", Description: "补足诅咒饰品以触发芜猴变化，或移除猴子诅咒", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "mode", Label: "操作", Type: "enum", Required: true, Options: []string{"add", "remove"}},
			},
			Script: `按 TMIR 规则补足诅咒饰品或清除猴子诅咒`, IsBuiltin: true,
		},
		render: renderMonkeyCurse,
	}
	templates["clear_player_debuffs"] = playerOnlyTemplate(
		"clear_player_debuffs", "清除玩家异常状态", "清除冻结、燃烧、昏睡和固定状态", RiskHigh,
		`local c=p.components or {};if c.freezable and c.freezable:IsFrozen()then c.freezable:Unfreeze()end;if c.burnable and c.burnable:IsBurning()then c.burnable:Extinguish();if c.firefx then c.firefx:Extinguish(true)end end;if c.grogginess then c.grogginess:ResetGrogginess();c.grogginess:ComeTo()end;if c.pinnable and c.pinnable:IsStuck()then c.pinnable:Unstick()end`,
	)
	templates["clear_player_naughtiness"] = template{
		definition: Definition{
			ID: "clear_player_naughtiness", Name: "设置淘气值事件", Description: "重置淘气值，或按 TMIR 语义生成一只或两至四只坎普斯", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "mode", Label: "操作", Type: "enum", Required: true, Options: []string{"reset", "spawn_one", "spawn_random"}},
			},
			Script: `按 KU ID 触发受限的 ms_forcenaughtiness 事件`, IsBuiltin: true,
		},
		render: renderPlayerNaughtiness,
	}
	templates["set_player_attack_multiplier"] = template{
		definition: Definition{
			ID: "set_player_attack_multiplier", Name: "设置玩家攻击倍率", Description: "设置玩家战斗组件的基础伤害倍率", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "multiplier", Label: "攻击倍率", Type: "number", Required: true, Minimum: &minAbilityValue, Maximum: &maxAbilityValue, Default: 1},
			},
			Script: `按 KU ID 设置 combat.damagemultiplier`, IsBuiltin: true,
		},
		render: renderPlayerAttackMultiplier,
	}
	templates["clear_player_inventory"] = template{
		definition: Definition{
			ID: "clear_player_inventory", Name: "清理玩家物品", Description: "清理玩家物品栏、背包或全部携带物品", Category: "玩家管理", Risk: RiskCritical,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "scope", Label: "清理范围", Type: "enum", Required: true, Options: []string{"inventory", "backpack", "all"}},
			},
			Script: `按 KU ID 查找玩家并清理指定物品容器`, IsBuiltin: true,
		},
		render: renderClearPlayerInventory,
	}
	templates["teleport_player"] = template{
		definition: Definition{
			ID: "teleport_player", Name: "传送玩家", Description: "将一名在线玩家传送到同一分片的另一名玩家身边", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "destination_id", Label: "目标玩家 KU ID", Type: "string", Required: true},
			},
			Script: `按两个 KU ID 查找玩家并设置安全坐标`, IsBuiltin: true,
		},
		render: renderTeleportPlayer,
	}
	templates["relocate_players"] = template{
		definition: Definition{
			ID: "relocate_players", Name: "移动在线玩家", Description: "召集当前分片玩家、召回另一玩家或互换两名玩家位置", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "主玩家 KU ID", Type: "string", Required: true},
				{Name: "action", Label: "移动方式", Type: "enum", Required: true, Options: []string{"gather", "forward", "recall", "swap"}},
				{Name: "other_player_id", Label: "另一玩家 KU ID", Type: "string", Description: "召集全部玩家时不需要"},
			},
			Script: `在当前分片按 KU ID 召集、传送、召回或互换玩家位置`, IsBuiltin: true,
		},
		render: renderRelocatePlayers,
	}
	templates["set_player_ability"] = template{
		definition: Definition{
			ID: "set_player_ability", Name: "设置玩家能力", Description: "开启或关闭玩家的常用调试能力", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "ability", Label: "能力", Type: "enum", Required: true, Options: []string{"no_cooldown", "one_hit_kill", "aoe_attack", "critical_work", "water_walk", "knockback_immunity", "no_hate", "stealth", "unlock_recipes"}},
				{Name: "enabled", Label: "启用", Type: "boolean", Required: true, Default: true},
				{Name: "value", Label: "能力参数", Type: "number", Minimum: &minAbilityValue, Maximum: &maxAbilityValue, Description: "范围攻击半径、工作倍率或潜行透明度"},
				{Name: "recipe_mode", Label: "配方解锁方式", Type: "enum", Options: []string{"temporary", "permanent"}, Default: "temporary"},
			},
			Script: `按 KU ID 设置白名单玩家能力`, IsBuiltin: true,
		},
		render: renderSetPlayerAbility,
	}
	templates["toggle_player_ghost"] = playerOnlyTemplate(
		"toggle_player_ghost", "切换幽灵状态", "在存活与幽灵状态之间切换玩家", RiskHigh,
		`if p:HasTag("playerghost")then p:PushEvent("respawnfromghost");p.rezsource="dst-admin-go"else p:PushEvent("death");p.deathpkname="dst-admin-go"end`,
	)
	templates["set_health_penalty"] = template{
		definition: Definition{
			ID: "set_health_penalty", Name: "调整生命上限惩罚", Description: "增加、减少或清除玩家的生命上限惩罚", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "mode", Label: "调整方式", Type: "enum", Required: true, Options: []string{"increase", "decrease", "clear"}},
			},
			Script: `通过 health:SetPenalty 调整生命上限惩罚`, IsBuiltin: true,
		},
		render: renderHealthPenalty,
	}
	templates["set_character_power"] = template{
		definition: Definition{
			ID: "set_character_power", Name: "设置角色专属能力", Description: "设置当前角色及其伙伴的专属资源或形态", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "action", Label: "角色能力", Type: "enum", Required: true, Options: []string{"beard_none", "beard_short", "beard_medium", "beard_long", "woby_empty", "woby_half", "woby_full", "abigail_bond_1", "abigail_bond_2", "abigail_bond_3", "abigail_health_5", "abigail_health_50", "abigail_health_100", "abigail_shadow", "abigail_lunar_toggle", "inspiration_0", "inspiration_50", "inspiration_100", "fitness_0", "fitness_50", "fitness_100", "woody_beaver", "woody_goose", "woody_moose", "wx_charge_add", "wx_charge_remove", "wormwood_bloom_0", "wormwood_bloom_1", "wormwood_bloom_2", "wormwood_bloom_3", "wormwood_bloom_progress_start", "wormwood_bloom_progress_half", "wormwood_bloom_progress_end", "wormwood_bloom_grow", "wormwood_bloom_decay", "merm_king_hunger_0", "merm_king_hunger_50", "merm_king_hunger_100", "merm_king_health_5", "merm_king_health_50", "merm_king_health_100", "merm_king_trident", "merm_king_crown", "merm_king_shoulder"}},
			},
			Script: `按 KU ID 与白名单动作设置角色专属组件`, IsBuiltin: true,
		},
		render: renderCharacterPower,
	}
	templates["manage_followers"] = template{
		definition: Definition{
			ID: "manage_followers", Name: "管理附近随从", Description: "招募、遣散、恢复或延长玩家附近随从状态", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "action", Label: "动作", Type: "enum", Required: true, Options: []string{"recruit", "dismiss", "heal", "feed", "loyal"}},
				{Name: "radius", Label: "半径", Type: "integer", Required: true, Minimum: &minRadius, Maximum: &maxRadius, Default: 25},
			},
			Script: `按玩家位置管理附近随从`, IsBuiltin: true,
		},
		render: renderManageFollowers,
	}
	templates["spawn_domesticated_beefalo"] = template{
		definition: Definition{
			ID: "spawn_domesticated_beefalo", Name: "生成驯化皮弗娄牛", Description: "在玩家附近生成指定倾向并配鞍的完全驯化皮弗娄牛", Category: "玩家管理", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "tendency", Label: "倾向", Type: "enum", Required: true, Options: []string{"default", "ornery", "pudgy", "rider", "custom"}},
				{Name: "saddle", Label: "鞍具", Type: "enum", Options: []string{"none", "shadow", "basic", "war", "race", "wathgrithr"}, Default: "none"},
				{Name: "bell", Label: "牛铃", Type: "enum", Options: []string{"none", "beef_bell", "shadow_beef_bell"}, Default: "none"},
				{Name: "domestication", Label: "驯化度", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 100},
				{Name: "hunger", Label: "饥饿度", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 50},
				{Name: "obedience", Label: "服从度", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 100},
				{Name: "health", Label: "生命值", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 100},
				{Name: "ornery", Label: "暴躁倾向", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 0},
				{Name: "rider", Label: "骑行倾向", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 0},
				{Name: "pudgy", Label: "肥胖倾向", Type: "number", Minimum: &minTimeScale, Maximum: &maxStat, Default: 0},
			},
			Script: `生成并完全驯化指定倾向的皮弗娄牛`, IsBuiltin: true,
		},
		render: renderSpawnDomesticatedBeefalo,
	}

	templates["skip_days"] = template{
		definition: Definition{ID: "skip_days", Name: "跳过天数", Description: "推进当前世界指定天数", Category: "世界控制", Risk: RiskHigh, Parameters: []Parameter{{Name: "days", Label: "天数", Type: "integer", Required: true, Minimum: &minDays, Maximum: &maxDays, Default: 1}}, Script: `LongUpdate(TUNING.TOTAL_DAY_TIME * {days})`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			days, err := intArgument(arguments, "days", 1, 200)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local d=%d;if LongUpdate and TUNING then LongUpdate(TUNING.TOTAL_DAY_TIME*d)else for i=1,d do TheWorld:PushEvent("ms_nextcycle")end end;print("[DST-ADMIN-SKIP-DAYS]",d)`, days), nil
		},
	}
	templates["set_time_scale"] = template{
		definition: Definition{ID: "set_time_scale", Name: "设置时间倍率", Description: "调整当前分片模拟时间倍率", Category: "世界控制", Risk: RiskHigh, Parameters: []Parameter{{Name: "multiplier", Label: "时间倍率", Type: "number", Required: true, Minimum: &minTimeScale, Maximum: &maxTimeScale, Default: 1}}, Script: `TheSim:SetTimeScale({multiplier})`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			multiplier, err := floatArgument(arguments, "multiplier", 0, 20)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`TheSim:SetTimeScale(%s);print("[DST-ADMIN-TIME-SCALE]",TheSim:GetTimeScale())`, luaNumber(multiplier)), nil
		},
	}
	templates["set_precipitation"] = template{
		definition: Definition{ID: "set_precipitation", Name: "设置降水", Description: "开始、停止或恢复动态降水", Category: "世界控制", Risk: RiskHigh, Parameters: []Parameter{{Name: "mode", Label: "降水模式", Type: "enum", Required: true, Options: []string{"start", "stop", "dynamic"}}}, Script: `设置 ms_setprecipitationmode 与 ms_forceprecipitation`, IsBuiltin: true},
		render:     renderSetPrecipitation,
	}
	templates["set_world_wetness"] = template{
		definition: Definition{ID: "set_world_wetness", Name: "设置世界湿度", Description: "将当前世界湿度调整到指定值", Category: "世界控制", Risk: RiskHigh, Parameters: []Parameter{{Name: "value", Label: "湿度", Type: "number", Required: true, Minimum: &minSpeed, Maximum: &maxStat, Default: 0}}, Script: `TheWorld:PushEvent("ms_deltawetness", target-current)`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			value, err := floatArgument(arguments, "value", 0, 100)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local target=%s;local current=(TheWorld.state and TheWorld.state.wetness)or 0;TheWorld:PushEvent("ms_deltawetness",target-current);print("[DST-ADMIN-WETNESS]",target)`, luaNumber(value)), nil
		},
	}
	templates["set_world_temperature"] = template{
		definition: Definition{
			ID: "set_world_temperature", Name: "设置世界温度", Description: "固定当前世界温度，或恢复游戏的自然温度变化", Category: "世界控制", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "mode", Label: "温度模式", Type: "enum", Required: true, Options: []string{"fixed", "dynamic"}},
				{Name: "value", Label: "固定温度", Type: "number", Minimum: &minWorldTemperature, Maximum: &maxWorldTemperature, Default: 20},
			},
			Script: `TheWorld.components.worldtemperature:SetTemperatureMod(multiplier, locus)`, IsBuiltin: true,
		},
		render: func(arguments map[string]interface{}) (string, error) {
			mode, err := enumArgument(arguments, "mode", "fixed", "dynamic")
			if err != nil {
				return "", err
			}
			if mode == "dynamic" {
				return `local c=TheWorld.components and TheWorld.components.worldtemperature;if c then c:SetTemperatureMod(1,0);print("[DST-ADMIN-WORLD-TEMPERATURE]","dynamic")end`, nil
			}
			value, err := floatArgument(arguments, "value", -25, 95)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local c=TheWorld.components and TheWorld.components.worldtemperature;local target=%s;if c then c:SetTemperatureMod(0,target);print("[DST-ADMIN-WORLD-TEMPERATURE]","fixed",target)end`, luaNumber(value)), nil
		},
	}
	templates["set_moon_phase"] = template{
		definition: Definition{ID: "set_moon_phase", Name: "设置月相", Description: "切换当前世界月相", Category: "世界控制", Risk: RiskHigh, Parameters: []Parameter{{Name: "phase", Label: "月相", Type: "enum", Required: true, Options: []string{"new", "quarter", "half", "threequarter", "full"}}}, Script: `TheWorld:PushEvent("ms_setmoonphase", ...)`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			phase, err := enumArgument(arguments, "phase", "new", "quarter", "half", "threequarter", "full")
			if err != nil {
				return "", err
			}
			return `TheWorld:PushEvent("ms_setmoonphase",{moonphase=` + quoteLua(phase) + `,iswaxing=true})`, nil
		},
	}
	templates["trigger_world_event"] = template{
		definition: Definition{
			ID: "trigger_world_event", Name: "触发世界事件", Description: "触发或切换环境、遗迹和噩梦周期事件", Category: "世界控制", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "event", Label: "事件", Type: "enum", Required: true, Options: []string{"lightning", "earthquake", "lunar_hail", "meteor_shower", "acid_rain", "moon_storm", "nightmare_calm", "nightmare_warn", "nightmare_wild", "nightmare_dawn", "reset_ruins"}},
				{Name: "player_id", Label: "定位玩家 KU ID", Type: "string", Description: "闪电和流星雨需要定位玩家"},
			},
			Script: `触发受限的世界事件`, IsBuiltin: true,
		},
		render: renderTriggerWorldEvent,
	}
	templates["set_special_event"] = template{
		definition: Definition{
			ID: "set_special_event", Name: "设置特殊活动", Description: "切换服务器当前季节活动或生肖活动", Category: "世界控制", Risk: RiskHigh,
			Parameters: []Parameter{{Name: "event", Label: "活动", Type: "enum", Required: true, Options: []string{"none", "default", "crow_carnival", "hallowed_nights", "winters_feast", "year_of_the_gobbler", "year_of_the_varg", "year_of_the_pig", "year_of_the_carrat", "year_of_the_beefalo", "year_of_the_catcoon", "year_of_the_bunnyman", "year_of_the_dragonfly", "year_of_the_snake", "year_of_the_knight"}}},
			Script:     `TheWorld:PushEvent("ms_setworldsetting", ...)`, IsBuiltin: true,
		},
		render: renderSetSpecialEvent,
	}
	templates["trigger_incident"] = template{
		definition: Definition{
			ID: "trigger_incident", Name: "触发突发事件", Description: "在玩家附近触发猎犬、蠕虫、青蛙雨、亮茄或海盗事件", Category: "世界控制", Risk: RiskHigh,
			Parameters: []Parameter{
				{Name: "player_id", Label: "定位玩家 KU ID", Type: "string", Required: true},
				{Name: "incident", Label: "突发事件", Type: "enum", Required: true, Options: []string{"hounds", "worms", "worm_boss", "frog_rain", "brightshade", "pirates"}},
			},
			Script: `按玩家位置触发白名单突发事件`, IsBuiltin: true,
		},
		render: renderTriggerIncident,
	}
	templates["act_nearby_entities"] = template{
		definition: Definition{
			ID: "act_nearby_entities", Name: "操作附近实体", Description: "按组件能力操作玩家附近实体，可选 Prefab 精确过滤", Category: "世界控制", Risk: RiskCritical,
			Parameters: []Parameter{
				{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true},
				{Name: "prefab", Label: "实体 Prefab 过滤", Type: "string", Description: "留空时操作附近所有适用实体"},
				{Name: "action", Label: "动作", Type: "enum", Required: true, Options: []string{"delete", "extinguish", "ignite", "restore", "repair", "repair_boat", "freshen", "heat", "cool", "salvage", "haunt", "fertilize", "grow", "harvest", "pick", "chop", "mine", "hammer", "dig", "till", "build_complete", "chaos", "electrocute", "kill", "freeze", "sleep", "panic", "pacify", "root", "taunt"}},
				{Name: "radius", Label: "半径", Type: "integer", Required: true, Minimum: &minRadius, Maximum: &maxRadius, Default: 10},
				{Name: "layout", Label: "耕地布局", Type: "enum", Options: []string{"2x2", "3x3", "4x4", "hexagon"}, Default: "3x3", Description: "仅耕地动作使用"},
			},
			Script: `按玩家位置、可选 Prefab 和组件能力执行白名单实体动作`, IsBuiltin: true,
		},
		render: renderNearbyEntityAction,
	}
}

func renderSetPlayerStat(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	stat, err := enumArgument(arguments, "stat", "health", "hunger", "sanity", "moisture", "temperature")
	if err != nil {
		return "", err
	}
	minimum, maximum := 0.0, 100.0
	if stat == "temperature" {
		minimum, maximum = -20, 90
	}
	value, err := floatArgument(arguments, "value", minimum, maximum)
	if err != nil {
		return "", err
	}
	setter := fmt.Sprintf(`p.components.%s:SetPercent(%s)`, stat, luaNumber(value/100))
	if stat == "health" {
		setter += `;if p:HasTag("health_as_oldage")then p:PushEvent("healthdelta")end`
	}
	if stat == "temperature" {
		setter = `p.components.temperature:SetTemperature(` + luaNumber(value) + `)`
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.%s then %s;print("[DST-ADMIN-PLAYER-STAT]",%s,%s,%s)else print("[DST-ADMIN-PLAYER-STAT]","PLAYER_OR_COMPONENT_NOT_FOUND",%s)end`, quoteLua(playerID), stat, setter, quoteLua(playerID), quoteLua(stat), luaNumber(value), quoteLua(playerID)), nil
}

func renderSetPlayerLock(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	resource, err := enumArgument(arguments, "resource", "health", "moisture", "temperature")
	if err != nil {
		return "", err
	}
	valueMode, err := optionalEnumArgument(arguments, "value_mode", "absolute", "absolute", "percent")
	if err != nil || (resource != "health" && valueMode != "absolute") {
		return "", ErrInvalidArguments
	}
	enabled, err := boolArgument(arguments, "enabled")
	if err != nil {
		return "", err
	}
	bounds := map[string][2]float64{"health": {1, 999}, "moisture": {0, 100}, "temperature": {-20, 90}}
	if resource == "health" && valueMode == "percent" {
		bounds[resource] = [2]float64{1, 100}
	}
	value, err := floatArgument(arguments, "value", bounds[resource][0], bounds[resource][1])
	if err != nil {
		return "", err
	}
	state := map[bool]string{true: "true", false: "false"}[enabled]
	actions := map[string]string{
		"health":      `local c=p.components and p.components.health;if c and not p:HasTag("playerghost")then if enabled then local lockvalue=value;if value_mode=="percent"then lockvalue=math.max(1,((c.GetMaxWithPenalty and c:GetMaxWithPenalty())or c.maxhealth or 1)*value/100)end;p._dst_admin_health_lock_enabled=true;p._dst_admin_health_lock_value=lockvalue;p._dst_admin_health_lock_mode=value_mode;c:SetMinHealth(lockvalue)else p._dst_admin_health_lock_enabled=nil;p._dst_admin_health_lock_value=nil;p._dst_admin_health_lock_mode=nil;c:SetMinHealth(0)end;applied=true end`,
		"moisture":    `local c=p.components and p.components.moisture;local function apply(inst)local m=inst.components and inst.components.moisture;if not m then return end;local pct=inst._dst_admin_moisture_lock_pct or 0;if pct<=0 and m.waterproofnessmodifiers then m.waterproofnessmodifiers:SetModifier("dst_admin_moisture_lock",TUNING.WATERPROOFNESS_ABSOLUTE)elseif m.waterproofnessmodifiers then m.waterproofnessmodifiers:RemoveModifier("dst_admin_moisture_lock")end;m:SetPercent(pct)end;if c and not p:HasTag("playerghost")then if p._dst_admin_moisture_lock_task then p._dst_admin_moisture_lock_task:Cancel();p._dst_admin_moisture_lock_task=nil end;if enabled then p._dst_admin_moisture_lock_pct=value/100;p._dst_admin_moisture_lock_task=p:DoPeriodicTask(.1,apply);apply(p)else if c.waterproofnessmodifiers then c.waterproofnessmodifiers:RemoveModifier("dst_admin_moisture_lock")end end;applied=true end`,
		"temperature": `local c=p.components and p.components.temperature;local function apply(inst)local t=inst.components and inst.components.temperature;if t then t:SetTemperature(inst._dst_admin_temp_lock_value or 35)end end;if c and not p:HasTag("playerghost")then if p._dst_admin_temp_lock_task then p._dst_admin_temp_lock_task:Cancel();p._dst_admin_temp_lock_task=nil end;if enabled then p._dst_admin_temp_lock_value=value;p._dst_admin_temp_lock_task=p:DoPeriodicTask(.1,apply);apply(p)else p._dst_admin_temp_lock_value=nil end;applied=true end`,
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local enabled=%s;local value=%s;local value_mode=%s;local applied=false;if p then %s end;print("[DST-ADMIN-PLAYER-LOCK]",%s,%s,enabled,value,value_mode,applied)`, quoteLua(playerID), state, luaNumber(value), quoteLua(valueMode), actions[resource], quoteLua(playerID), quoteLua(resource)), nil
}

func renderPlayerNaughtiness(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	mode, err := enumArgument(arguments, "mode", "reset", "spawn_one", "spawn_random")
	if err != nil {
		return "", err
	}
	spawns := map[string]string{"reset": "0", "spawn_one": "1", "spawn_random": "math.random(2,4)"}[mode]
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p then local n=%s;TheWorld:PushEvent("ms_forcenaughtiness",{player=p,numspawns=n});print("[DST-ADMIN-NAUGHTINESS]",%s,%s,n)else print("[DST-ADMIN-NAUGHTINESS]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), spawns, quoteLua(playerID), quoteLua(mode), quoteLua(playerID)), nil
}

func renderPlayerAttackMultiplier(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	multiplier, err := floatArgument(arguments, "multiplier", 0, 99999)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local c=p and p.components and p.components.combat;if c then c.damagemultiplier=%s;print("[DST-ADMIN-ATTACK-MULTIPLIER]",%s,%s)else print("[DST-ADMIN-ATTACK-MULTIPLIER]","PLAYER_OR_COMPONENT_NOT_FOUND",%s)end`, quoteLua(playerID), luaNumber(multiplier), quoteLua(playerID), luaNumber(multiplier), quoteLua(playerID)), nil
}

func renderAdjustPlayerLuck(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	mode, err := enumArgument(arguments, "mode", "add", "subtract", "clear")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local applied=false;local luck=nil;if p and p.components and p.components.luckuser then local c=p.components.luckuser;local src="dst-admin-go";local mode=%s;if mode=="clear"then c:RemoveLuckSource(src);applied=true elseif c.luckmodifiers then local current=c.luckmodifiers:CalculateModifierFromSource(src,src)or 0;c:SetLuckSource(current+(mode=="add"and .5 or-.5),src);applied=true end;if c.GetLuck then luck=c:GetLuck()end end;print("[DST-ADMIN-PLAYER-LUCK]",%s,%s,applied,luck)`, quoteLua(playerID), quoteLua(mode), quoteLua(playerID), quoteLua(mode)), nil
}

func renderManagePlayerSkills(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	mode, err := enumArgument(arguments, "mode", "grant_xp", "reset")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local success=false;if p and p.components and p.components.skilltreeupdater then local u=p.components.skilltreeupdater;local defs=require("prefabs/skilltree_defs").SKILLTREE_DEFS[p.prefab];if defs then if %s=="grant_xp"then u:AddSkillXP(9999999);success=true else local attempts=50;while attempts>0 do local changed=false;for skill,data in pairs(defs)do if data.rpc_id and u:IsActivated(skill)then u:DeactivateSkill(skill);changed=true end end;if not changed then break end;attempts=attempts-1 end;success=true;for skill,data in pairs(defs)do if data.rpc_id and u:IsActivated(skill)then success=false;break end end end end end;print("[DST-ADMIN-PLAYER-SKILLS]",%s,%s,success)`, quoteLua(playerID), quoteLua(mode), quoteLua(playerID), quoteLua(mode)), nil
}

func renderMonkeyCurse(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	mode, err := enumArgument(arguments, "mode", "add", "remove")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local success=false;if p and p.components and p.components.cursable and p.components.inventory then local c,inv=p.components.cursable,p.components.inventory;if %s=="remove"then if inv:FindItem(function(item)return item:HasTag("monkey_token")end)then c:RemoveCurse("MONKEY",TUNING.STACK_SIZE_LARGEITEM*(inv:GetNumSlots()+1));success=true end else local count=0;for _,item in pairs(inv.itemslots or {})do if item:HasTag("monkey_token")and item.components and item.components.stackable then count=count+item.components.stackable:StackSize()end end;local active=inv.activeitem;if active and active:HasTag("monkey_token")and active.components and active.components.stackable then count=count+active.components.stackable:StackSize()end;if p.prefab~="wonkey"and count<10 then for i=1,10-count do inv:GiveItem(SpawnPrefab("cursed_monkey_token"))end;success=true end end end;print("[DST-ADMIN-MONKEY-CURSE]",%s,%s,success)`, quoteLua(playerID), quoteLua(mode), quoteLua(playerID), quoteLua(mode)), nil
}

func playerOnlyTemplate(id, name, description string, risk Risk, action string) template {
	return template{
		definition: Definition{ID: id, Name: name, Description: description, Category: "玩家管理", Risk: risk, Parameters: []Parameter{{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true}}, Script: `按 KU ID 查找玩家并执行受限维护动作`, IsBuiltin: true},
		render: func(arguments map[string]interface{}) (string, error) {
			playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p then %s;print("[DST-ADMIN-PLAYER-ACTION]",%s,%s)else print("[DST-ADMIN-PLAYER-ACTION]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), action, quoteLua(id), quoteLua(playerID), quoteLua(playerID)), nil
		},
	}
}

func renderClearPlayerInventory(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	scope, err := enumArgument(arguments, "scope", "inventory", "backpack", "all")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.inventory then local inv=p.components.inventory;local scope=%s;local n,curse=0,0;local function remove(item)if item then if item:HasTag("monkey_token")and item.components and item.components.stackable then curse=curse+item.components.stackable:StackSize()end;inv:RemoveItem(item,true);item:Remove();n=n+1 end end;if scope=="inventory"or scope=="all"then for i=1,inv:GetNumSlots()do remove(inv:GetItemInSlot(i))end end;if scope=="all"then remove(inv.activeitem)end;if scope=="backpack"or scope=="all"then local bag=inv:GetOverflowContainer();if bag then for i=1,bag:GetNumSlots()do remove(bag:GetItemInSlot(i))end end end;if curse>0 and p.components.cursable then p.components.cursable:RemoveCurse("MONKEY",curse)end;print("[DST-ADMIN-CLEAR-INVENTORY]",%s,scope,n)else print("[DST-ADMIN-CLEAR-INVENTORY]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), quoteLua(scope), quoteLua(playerID), quoteLua(playerID)), nil
}

func renderTeleportPlayer(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	destinationID, err := matchedArgument(arguments, "destination_id", dstUserIDPattern)
	if err != nil || destinationID == playerID {
		return "", ErrInvalidArguments
	}
	return fmt.Sprintf(`local p,d=nil,nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v elseif v.userid==%s then d=v end end;if p and d and p.Transform and d.Transform then local x,y,z=d.Transform:GetWorldPosition();p.Transform:SetPosition(x,y,z);print("[DST-ADMIN-TELEPORT]",%s,%s)else print("[DST-ADMIN-TELEPORT]","PLAYER_NOT_FOUND",%s,%s)end`, quoteLua(playerID), quoteLua(destinationID), quoteLua(playerID), quoteLua(destinationID), quoteLua(playerID), quoteLua(destinationID)), nil
}

func renderRelocatePlayers(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	action, err := enumArgument(arguments, "action", "gather", "forward", "recall", "swap")
	if err != nil {
		return "", err
	}
	otherID, err := optionalMatchedArgument(arguments, "other_player_id", dstUserIDPattern)
	if err != nil || (action != "gather" && (otherID == "" || otherID == playerID)) {
		return "", ErrInvalidArguments
	}
	actions := map[string]string{
		"gather":  `if p then local x,y,z=position(p);if x then for _,v in ipairs(AllPlayers or {})do if v~=p and v:IsValid()then teleport(v,x,y,z);moved=moved+1 end end end end`,
		"forward": `if p and other then local x,y,z=position(other);if x then teleport(p,x,y,z);moved=1 end end`,
		"recall":  `if p and other then local x,y,z=position(p);if x then teleport(other,x,y,z);moved=1 end end`,
		"swap":    `if p and other then local x1,y1,z1=position(p);local x2,y2,z2=position(other);if x1 and x2 then teleport(p,x2,y2,z2);teleport(other,x1,y1,z1);moved=2 end end`,
	}
	return fmt.Sprintf(`local p,other=nil,nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v elseif v.userid==%s then other=v end end;local function position(inst)if inst and inst.Transform then return inst.Transform:GetWorldPosition()end end;local function teleport(inst,x,y,z)if inst.Physics then inst.Physics:Teleport(x,y,z)elseif inst.Transform then inst.Transform:SetPosition(x,y,z)end end;local moved=0;%s;print("[DST-ADMIN-RELOCATE]",%s,%s,%s,moved)`, quoteLua(playerID), quoteLua(otherID), actions[action], quoteLua(playerID), quoteLua(otherID), quoteLua(action)), nil
}

func renderSetPlayerAbility(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	ability, err := enumArgument(arguments, "ability", "no_cooldown", "one_hit_kill", "aoe_attack", "critical_work", "water_walk", "knockback_immunity", "no_hate", "stealth", "unlock_recipes")
	if err != nil {
		return "", err
	}
	enabled, err := boolArgument(arguments, "enabled")
	if err != nil {
		return "", err
	}
	state := "false"
	if enabled {
		state = "true"
	}
	valueBounds := map[string][3]float64{
		"aoe_attack":    {4, 1, 40},
		"critical_work": {99999, 0, 99999},
		"stealth":       {0, 0, 100},
	}
	value := 0.0
	if bounds, exists := valueBounds[ability]; exists {
		value, err = optionalFloatArgument(arguments, "value", bounds[0], bounds[1], bounds[2])
		if err != nil {
			return "", err
		}
	}
	recipeMode, err := optionalEnumArgument(arguments, "recipe_mode", "temporary", "temporary", "permanent")
	if err != nil {
		return "", err
	}
	actions := map[string]string{
		"no_cooldown":        `if enabled then if p._dst_admin_nocooldown_task then p._dst_admin_nocooldown_task:Cancel()end;p:AddTag("dst_admin_nocooldown");p._dst_admin_nocooldown_task=p:DoPeriodicTask(.1,function(inst)local inv=inst.components and inst.components.inventory;if not inv then return end;if inst.HasDebuff and inst:HasDebuff("wurt_terraform_cast_debuff")then inst:RemoveDebuff("wurt_terraform_cast_debuff")end;local function reset(item)if item and item.components then local r=item.components.rechargeable;if r then r:SetCharge(r.total);r:SetChargeTime(0)end;local c=item.components.cooldown;if c then c:FinishCharging()end end end;for _,item in pairs(inv.itemslots or {})do reset(item)end;for _,item in pairs(inv.equipslots or {})do reset(item)end;reset(inv.activeitem);local bag=inv:GetOverflowContainer();if bag then for i=1,bag:GetNumSlots()do reset(bag:GetItemInSlot(i))end end;local s=inst.components.spellbookcooldowns;if s then for name in pairs(s.cooldowns or {})do s:StopSpellCooldown(name)end end;local wx=inst.components.wx78_abilitycooldowns;if wx then for _,cd in pairs(wx.cooldowns or {})do cd:Remove()end end end)else p:RemoveTag("dst_admin_nocooldown");if p._dst_admin_nocooldown_task then p._dst_admin_nocooldown_task:Cancel();p._dst_admin_nocooldown_task=nil end end`,
		"one_hit_kill":       `local c=p.components and p.components.combat;if c then if enabled and not c._dst_admin_calc_damage then c._dst_admin_calc_damage=c.CalcDamage;c.CalcDamage=function(self,target,...)if target and target:HasTag("alwaysblock")then return 0 end;return 99999999999 end elseif not enabled and c._dst_admin_calc_damage then c.CalcDamage=c._dst_admin_calc_damage;c._dst_admin_calc_damage=nil end end`,
		"aoe_attack":         `local c=p.components and p.components.combat;if c then if enabled then if not c._dst_admin_aoe then c._dst_admin_aoe={range=c.areahitrange,percent=c.areahitdamagepercent,check=c.areahitcheck,disabled=c.areahitdisabled}end;c.areahitrange=value;c.areahitdamagepercent=1;c.areahitcheck=nil;c.areahitdisabled=false elseif c._dst_admin_aoe then c.areahitrange=c._dst_admin_aoe.range;c.areahitdamagepercent=c._dst_admin_aoe.percent;c.areahitcheck=c._dst_admin_aoe.check;c.areahitdisabled=c._dst_admin_aoe.disabled;c._dst_admin_aoe=nil end end`,
		"critical_work":      `local w=p.components and p.components.workmultiplier;if w then if enabled then if w._dst_admin_specialfn==nil then w._dst_admin_specialfn=w.specialfn or false end;w._dst_admin_work_multiplier=value;w:SetSpecialMultiplierFn(function(inst,action,target,tool,numworks,recoil)if not recoil and numworks~=0 and value>=99999 and inst.player_classified then inst.player_classified.playworkcritsound:push()end;return value end)elseif w._dst_admin_specialfn~=nil then w:SetSpecialMultiplierFn(w._dst_admin_specialfn~=false and w._dst_admin_specialfn or nil);w._dst_admin_specialfn=nil;w._dst_admin_work_multiplier=nil end end`,
		"water_walk":         `local function apply(inst)inst:AddTag("dst_admin_waterwalk");if inst.Physics then inst.Physics:ClearCollidesWith(COLLISION.LIMITS)end;if inst.components and inst.components.drownable then if not inst._dst_admin_shoulddrown then inst._dst_admin_shoulddrown=inst.components.drownable.ShouldDrown end;inst.components.drownable.ShouldDrown=function()return false end end end;local function clear(inst)inst:RemoveTag("dst_admin_waterwalk");if inst.Physics then if ChangeToCharacterPhysics then ChangeToCharacterPhysics(inst)else inst.Physics:CollidesWith(COLLISION.LIMITS)end end;if inst.components and inst.components.drownable and inst._dst_admin_shoulddrown then inst.components.drownable.ShouldDrown=inst._dst_admin_shoulddrown;inst._dst_admin_shoulddrown=nil end end;if enabled then p:AddTag("dst_admin_waterwalk_mode");apply(p)else p:RemoveTag("dst_admin_waterwalk_mode");if p:HasTag("dst_admin_god_mode")then apply(p)else clear(p)end end`,
		"knockback_immunity": `local function nonslip(inst,on)local c=inst.components or {};if on and c.slipperyfeet then local u=c.nonslipgrituser;if not u and inst.AddComponent then pcall(inst.AddComponent,inst,"nonslipgrituser");u=inst.components.nonslipgrituser end;if u and u.AddSource then u:AddSource(inst,"dst_admin_knockback");inst._dst_admin_nonslip=true;if c.slipperyfeet.SetCurrent then c.slipperyfeet:SetCurrent(0)end end elseif inst._dst_admin_nonslip and c.nonslipgrituser and c.nonslipgrituser.RemoveSource then c.nonslipgrituser:RemoveSource(inst,"dst_admin_knockback");inst._dst_admin_nonslip=nil end end;if enabled then if not p._dst_admin_mass and p.Physics then p._dst_admin_mass=p.Physics:GetMass();p.Physics:SetMass(p._dst_admin_mass+99999)end;p:AddTag("heavybody");nonslip(p,true);if p.components and p.components.combat then if p._dst_admin_stunlock==nil then p._dst_admin_stunlock=p.components.combat.playerstunlock end;p.components.combat.playerstunlock=PLAYERSTUNLOCK.NEVER end;if p._dst_admin_knockback_task then p._dst_admin_knockback_task:Cancel()end;p._dst_admin_knockback_task=p:DoPeriodicTask(0,function(inst)if inst.sg and inst.sg:HasStateTag("knockback")then inst.sg:GoToState("idle",true)elseif inst.sg and inst.sg.currentstate then local n=inst.sg.currentstate.name;if n=="slip"or n=="slip_pst"or n=="slip_fall"or n=="slip_fall_loop"or n=="slip_fall_pst"then inst.sg:GoToState("idle",true)end end end)else if p._dst_admin_mass and p.Physics then p.Physics:SetMass(p._dst_admin_mass);p._dst_admin_mass=nil end;p:RemoveTag("heavybody");nonslip(p,false);if p.components and p.components.combat and p._dst_admin_stunlock~=nil then p.components.combat.playerstunlock=p._dst_admin_stunlock;p._dst_admin_stunlock=nil end;if p._dst_admin_knockback_task then p._dst_admin_knockback_task:Cancel();p._dst_admin_knockback_task=nil end end`,
		"no_hate":            `local function clear(inst)local x,y,z=inst.Transform:GetWorldPosition();for _,e in pairs(TheSim:FindEntities(x,y,z,25,nil,{"INLIMBO"}))do if e~=inst and e~=TheWorld and e.components and e.components.combat and e.components.combat.target==inst then e.components.combat:SetTarget(nil);e.components.combat:GiveUp()end end end;if enabled then if not p:HasTag("dst_admin_no_hate")then p._dst_admin_nohate_had_debug=p:HasTag("debugnoattack")end;p:AddTag("dst_admin_no_hate");p:AddTag("debugnoattack");clear(p);if p._dst_admin_nohate_task then p._dst_admin_nohate_task:Cancel()end;p._dst_admin_nohate_task=p:DoPeriodicTask(1,clear)else p:RemoveTag("dst_admin_no_hate");if not p._dst_admin_nohate_had_debug and not p:HasTag("dst_admin_stealth_mode")then p:RemoveTag("debugnoattack")end;p._dst_admin_nohate_had_debug=nil;if p._dst_admin_nohate_task then p._dst_admin_nohate_task:Cancel();p._dst_admin_nohate_task=nil end end`,
		"stealth":            `local tags={"notarget","invisible","noplayerindicator","debugnoattack","mime","NOCLICK"};local function clearhate(inst)local x,y,z=inst.Transform:GetWorldPosition();if inst.components and inst.components.combat then inst.components.combat:SetTarget(nil)end;for _,e in pairs(TheSim:FindEntities(x,y,z,40,nil,{"INLIMBO"}))do if e~=inst and e~=TheWorld and e.components and e.components.combat and e.components.combat.target==inst then e.components.combat:SetTarget(nil);e.components.combat:GiveUp()end end end;if enabled then if not p:HasTag("dst_admin_stealth_mode")then p._dst_admin_stealth_tags={};for _,tag in ipairs(tags)do p._dst_admin_stealth_tags[tag]=p:HasTag(tag)end end;p:AddTag("dst_admin_stealth_mode");for _,tag in ipairs(tags)do p:AddTag(tag)end;p._dst_admin_stealth_alpha=value/100;if p.components.health then p.components.health:SetInvincible(true)end;p:Show();if p.AnimState then p.AnimState:SetMultColour(1,1,1,p._dst_admin_stealth_alpha);if p._dst_admin_stealth_alpha<=0 then p:Hide()end end;if p.DynamicShadow then p.DynamicShadow:Enable(false)end;if p.MiniMapEntity then p.MiniMapEntity:SetEnabled(false)end;if p.SoundEmitter then p.SoundEmitter:SetMute(true)end;if p.components.locomotor then p.components.locomotor:SetTriggersCreep(false)end;clearhate(p);if p._dst_admin_stealth_task then p._dst_admin_stealth_task:Cancel()end;p._dst_admin_stealth_task=p:DoPeriodicTask(1,clearhate)else p:RemoveTag("dst_admin_stealth_mode");p:Show();for _,tag in ipairs(tags)do if not(p._dst_admin_stealth_tags and p._dst_admin_stealth_tags[tag])then if tag~="debugnoattack"or not p:HasTag("dst_admin_no_hate")then p:RemoveTag(tag)end end end;p._dst_admin_stealth_tags=nil;p._dst_admin_stealth_alpha=nil;if p.AnimState then p.AnimState:SetMultColour(1,1,1,1)end;if p.components.health and not p._dst_admin_god_task then p.components.health:SetInvincible(false)end;if p.DynamicShadow then p.DynamicShadow:Enable(true)end;if p.MiniMapEntity then p.MiniMapEntity:SetEnabled(true)end;if p.SoundEmitter then p.SoundEmitter:SetMute(false)end;if p.components.locomotor then p.components.locomotor:SetTriggersCreep(not p:HasTag("spiderwhisperer"))end;if p._dst_admin_stealth_task then p._dst_admin_stealth_task:Cancel();p._dst_admin_stealth_task=nil end end`,
		"unlock_recipes":     `local b=p.components and p.components.builder;local r=p.replica and p.replica.builder;local function giveall()for _,recipe in pairs(AllRecipes or {})do local name=recipe.name;if GetValidRecipe(name)and not table.contains(b.recipes,name)then table.insert(b.recipes,name);if r then r:AddRecipe(name)end;p:PushEvent("unlockrecipe",{recipe=name})end end end;local function restoretemp()if not b._dst_admin_recipes then return end;for i=#b.recipes,1,-1 do local name=table.remove(b.recipes,i);if r then r:RemoveRecipe(name)end end;for _,name in pairs(b._dst_admin_recipes)do if GetValidRecipe(name)then table.insert(b.recipes,name);if r then r:AddRecipe(name)end;p:PushEvent("unlockrecipe",{recipe=name})end end;b.OnSave=b._dst_admin_oldonsave;b._dst_admin_oldonsave=nil;b._dst_admin_recipes=nil end;if b then if enabled and recipe_mode=="permanent"then if b._dst_admin_recipes then restoretemp()end;giveall()elseif enabled then if not b._dst_admin_recipes then b._dst_admin_recipes=shallowcopy(b.recipes);b._dst_admin_oldonsave=b.OnSave;b.OnSave=function(self,...)local result=self._dst_admin_oldonsave and self._dst_admin_oldonsave(self,...)or{};result.recipes=self._dst_admin_recipes;return result end end;giveall()elseif b._dst_admin_recipes then restoretemp()end end`,
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local enabled=%s;local value=%s;local recipe_mode=%s;if p then %s;print("[DST-ADMIN-PLAYER-ABILITY]",%s,%s,enabled,value,recipe_mode)else print("[DST-ADMIN-PLAYER-ABILITY]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), state, luaNumber(value), quoteLua(recipeMode), actions[ability], quoteLua(playerID), quoteLua(ability), quoteLua(playerID)), nil
}

func renderHealthPenalty(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	mode, err := enumArgument(arguments, "mode", "increase", "decrease", "clear")
	if err != nil {
		return "", err
	}
	delta := map[string]string{"increase": ".25", "decrease": "-.25", "clear": "0"}[mode]
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.components and p.components.health then local h=p.components.health;local value=%s;if %s=="clear"then h:SetPenalty(0)elseif not h.disable_penalty and not p:HasTag("playerghost")then h:SetPenalty(math.clamp((h.penalty or 0)+value,0,.75))end;h:ForceUpdateHUD(true);print("[DST-ADMIN-HEALTH-PENALTY]",%s,h.penalty or 0)else print("[DST-ADMIN-HEALTH-PENALTY]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), delta, quoteLua(mode), quoteLua(playerID), quoteLua(playerID)), nil
}

func renderCharacterPower(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	action, err := enumArgument(arguments, "action", "beard_none", "beard_short", "beard_medium", "beard_long", "woby_empty", "woby_half", "woby_full", "abigail_bond_1", "abigail_bond_2", "abigail_bond_3", "abigail_health_5", "abigail_health_50", "abigail_health_100", "abigail_shadow", "abigail_lunar_toggle", "inspiration_0", "inspiration_50", "inspiration_100", "fitness_0", "fitness_50", "fitness_100", "woody_beaver", "woody_goose", "woody_moose", "wx_charge_add", "wx_charge_remove", "wormwood_bloom_0", "wormwood_bloom_1", "wormwood_bloom_2", "wormwood_bloom_3", "wormwood_bloom_progress_start", "wormwood_bloom_progress_half", "wormwood_bloom_progress_end", "wormwood_bloom_grow", "wormwood_bloom_decay", "merm_king_hunger_0", "merm_king_hunger_50", "merm_king_hunger_100", "merm_king_health_5", "merm_king_health_50", "merm_king_health_100", "merm_king_trident", "merm_king_crown", "merm_king_shoulder")
	if err != nil {
		return "", err
	}
	actions := map[string]string{
		"beard_none":                    `local c=p.components and p.components.beard;if c and c.isgrowing then c.daysgrowth=0;c:Reset();applied=true end`,
		"beard_short":                   `local c=p.components and p.components.beard;if c and c.isgrowing then c.daysgrowth=4;c:SetSkin(c.skinname);c:UpdateBeardInventory();applied=true end`,
		"beard_medium":                  `local c=p.components and p.components.beard;if c and c.isgrowing then c.daysgrowth=8;c:SetSkin(c.skinname);c:UpdateBeardInventory();applied=true end`,
		"beard_long":                    `local c=p.components and p.components.beard;if c and c.isgrowing then c.daysgrowth=16;c:SetSkin(c.skinname);c:UpdateBeardInventory();applied=true end`,
		"woby_empty":                    characterPercentScript(`p.woby and p.woby.components.hunger`, 0),
		"woby_half":                     characterPercentScript(`p.woby and p.woby.components.hunger`, .5),
		"woby_full":                     characterPercentScript(`p.woby and p.woby.components.hunger`, 1),
		"abigail_bond_1":                `local c=p.components and p.components.ghostlybond;if c then c:SetBondLevel(1);applied=true end`,
		"abigail_bond_2":                `local c=p.components and p.components.ghostlybond;if c then c:SetBondLevel(2);applied=true end`,
		"abigail_bond_3":                `local c=p.components and p.components.ghostlybond;if c then c:SetBondLevel(3);applied=true end`,
		"abigail_health_5":              characterPercentScript(`p.components and p.components.ghostlybond and p.components.ghostlybond.ghost and p.components.ghostlybond.ghost.components.health`, .05),
		"abigail_health_50":             characterPercentScript(`p.components and p.components.ghostlybond and p.components.ghostlybond.ghost and p.components.ghostlybond.ghost.components.health`, .5),
		"abigail_health_100":            characterPercentScript(`p.components and p.components.ghostlybond and p.components.ghostlybond.ghost and p.components.ghostlybond.ghost.components.health`, 1),
		"abigail_shadow":                `local c=p.components and p.components.ghostlybond;if c and c.ghost and c.ghost.DoShadowBurstBuff then c.ghost:DoShadowBurstBuff(20);applied=true end`,
		"abigail_lunar_toggle":          `local c=p.components and p.components.ghostlybond;if c and c.ghost then c.ghost:PushEvent("gestalt_mutate",{gestalt=not c.ghost:HasTag("gestalt")});applied=true end`,
		"inspiration_0":                 characterPercentScript(`p.components and p.components.singinginspiration`, 0),
		"inspiration_50":                characterPercentScript(`p.components and p.components.singinginspiration`, .5),
		"inspiration_100":               characterPercentScript(`p.components and p.components.singinginspiration`, 1),
		"fitness_0":                     characterPercentScript(`p.components and p.components.mightiness`, 0),
		"fitness_50":                    characterPercentScript(`p.components and p.components.mightiness`, .5),
		"fitness_100":                   `local c=p.components and p.components.mightiness;if c then c:DoDelta(c:GetMax()+c:GetOverMax()-c:GetCurrent(),nil,nil,nil,true);applied=true end`,
		"woody_beaver":                  characterWereScript("beaver"),
		"woody_goose":                   characterWereScript("goose"),
		"woody_moose":                   characterWereScript("moose"),
		"wx_charge_add":                 `local c=p.components and p.components.upgrademoduleowner;if c then c:AddCharge(1);applied=true end`,
		"wx_charge_remove":              `local c=p.components and p.components.upgrademoduleowner;if c then c:AddCharge(-1);applied=true end`,
		"wormwood_bloom_0":              characterBloomScript(0),
		"wormwood_bloom_1":              characterBloomScript(1),
		"wormwood_bloom_2":              characterBloomScript(2),
		"wormwood_bloom_3":              characterBloomScript(3),
		"wormwood_bloom_progress_start": characterBloomProgressScript(.001),
		"wormwood_bloom_progress_half":  characterBloomProgressScript(.5),
		"wormwood_bloom_progress_end":   characterBloomProgressScript(.999),
		"wormwood_bloom_grow":           characterBloomDirectionScript(true),
		"wormwood_bloom_decay":          characterBloomDirectionScript(false),
		"merm_king_hunger_0":            characterMermKingScript("hunger", 0),
		"merm_king_hunger_50":           characterMermKingScript("hunger", .5),
		"merm_king_hunger_100":          characterMermKingScript("hunger", 1),
		"merm_king_health_5":            characterMermKingScript("health", .05),
		"merm_king_health_50":           characterMermKingScript("health", .5),
		"merm_king_health_100":          characterMermKingScript("health", 1),
		"merm_king_trident":             characterMermKingGearScript("trident", "trident", "trident", "get_trident", "onmermkingtridentadded", "onmermkingtridentremoved"),
		"merm_king_crown":               characterMermKingGearScript("ruinshat", "crown", "crown", "get_crown", "onmermkingcrownadded", "onmermkingcrownremoved"),
		"merm_king_shoulder":            characterMermKingGearScript("armormarble", "shoulder_lilly", "shoulder_lilly", "get_pauldron", "onmermkingpauldronadded", "onmermkingpauldronremoved"),
	}
	prefabByAction := map[string]string{}
	for _, prefix := range []struct {
		name   string
		prefab string
	}{
		{"beard_", "wilson"}, {"woby_", "walter"}, {"abigail_", "wendy"}, {"inspiration_", "wigfrid"},
		{"fitness_", "wolfgang"}, {"woody_", "woodie"}, {"wx_", "wx78"}, {"wormwood_", "wormwood"}, {"merm_king_", "wurt"},
	} {
		if len(action) >= len(prefix.name) && action[:len(prefix.name)] == prefix.name {
			prefabByAction[action] = prefix.prefab
			break
		}
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local applied=false;if p and p.prefab==%s and not p:HasTag("playerghost")then %s end;print("[DST-ADMIN-CHARACTER-POWER]",%s,%s,applied)`, quoteLua(playerID), quoteLua(prefabByAction[action]), actions[action], quoteLua(playerID), quoteLua(action)), nil
}

func characterPercentScript(component string, percent float64) string {
	return `local c=` + component + `;if c then c:SetPercent(` + luaNumber(percent) + `);applied=true end`
}

func characterWereScript(mode string) string {
	return `local c=p.components and p.components.wereness;if c then c:SetWereMode(` + quoteLua(mode) + `);c:SetPercent(1);applied=true end`
}

func characterBloomScript(level int) string {
	if level == 0 {
		return `local c=p.components and p.components.bloomness;if c then c.level=0;c.timer=0;c.is_blooming=false;c:UpdateRate();if p.StopUpdatingComponent then p:StopUpdatingComponent(c)end;if c.onlevelchangedfn then c.onlevelchangedfn(p,0)end;applied=true end`
	}
	return fmt.Sprintf(`local c=p.components and p.components.bloomness;if c then local waszero=(c.level or 0)==0;c.fertilizer=0;c.level=%d;c.is_blooming=%s;local duration=c.level<c.max and c.stage_duration or(TUNING.WORMWOOD_BLOOM_FULL_MAX_DURATION or c.full_bloom_duration);c.timer=duration or 0;c:UpdateRate();if waszero and p.StartUpdatingComponent then p:StartUpdatingComponent(c)end;if c.onlevelchangedfn then c.onlevelchangedfn(p,%d)end;applied=true end`, level, map[bool]string{true: "false", false: "true"}[level >= 3], level)
}

func characterBloomProgressScript(percent float64) string {
	return `local c=p.components and p.components.bloomness;if c and(c.level or 0)>0 then local duration=c.level<c.max and c.stage_duration or(TUNING.WORMWOOD_BLOOM_FULL_MAX_DURATION or c.full_bloom_duration);if duration and duration>0 then c.fertilizer=0;local pct=` + luaNumber(percent) + `;c.timer=c.is_blooming and(duration*(1-pct))or(duration*pct);c:UpdateRate();applied=true end end`
}

func characterBloomDirectionScript(growing bool) string {
	return `local c=p.components and p.components.bloomness;if c and(c.level or 0)>0 and c.level<c.max then c.is_blooming=` + map[bool]string{true: "true", false: "false"}[growing] + `;c:UpdateRate();applied=true end`
}

func characterMermKingScript(component string, percent float64) string {
	prefix := ""
	if component == "hunger" {
		prefix = `if king and king.RefreshHungerParameters then king:RefreshHungerParameters(p)end;`
	}
	return `local m=TheWorld.components and TheWorld.components.mermkingmanager;local king=m and m.GetKing and m:GetKing();` + prefix + `local c=king and king.components and king.components.` + component + `;if c and(not king.components.health or not king.components.health:IsDead())then c:SetPercent(` + luaNumber(percent) + `);applied=true end`
}

func characterMermKingGearScript(prefab, symbol, override, state, addEvent, removeEvent string) string {
	return `local m=TheWorld.components and TheWorld.components.mermkingmanager;local king=m and m.GetKing and m:GetKing();local inv=king and king.components and king.components.inventory;if king and inv and king.components.health and not king.components.health:IsDead()then local item=inv:FindItem(function(v)return v.prefab==` + quoteLua(prefab) + ` end);if item then inv:RemoveItem(item,true);if item:IsValid()then item:Remove()end;if king.AnimState then king.AnimState:ClearOverrideSymbol(` + quoteLua(symbol) + `)end;TheWorld:PushEvent(` + quoteLua(removeEvent) + `);applied=true else item=SpawnPrefab(` + quoteLua(prefab) + `);if item then inv:GiveItem(item);if king.AnimState then king.AnimState:OverrideSymbol(` + quoteLua(symbol) + `,"mermkingswaps",` + quoteLua(override) + `)end;if king.sg and not king.sg:HasStateTag("busy")then king.sg:GoToState(` + quoteLua(state) + `)end;TheWorld:PushEvent(` + quoteLua(addEvent) + `);applied=true end end end`
}

func renderManageFollowers(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	action, err := enumArgument(arguments, "action", "recruit", "dismiss", "heal", "feed", "loyal")
	if err != nil {
		return "", err
	}
	radius, err := intArgument(arguments, "radius", 1, 64)
	if err != nil {
		return "", err
	}
	actions := map[string]string{
		"recruit": `local f=e.components and e.components.follower;local clockwork=e.prefab=="knight"or e.prefab=="knight_nightmare"or e.prefab=="bishop"or e.prefab=="bishop_nightmare"or e.prefab=="rook"or e.prefab=="rook_nightmare";local shocked=clockwork and e.sg and(e.sg:HasStateTag("electrocute")or(e.sg.currentstate and e.sg.currentstate.name=="electrocute"));if e~=p and not e:HasTag("player")and p.components and p.components.leader and(f or shocked)then if not f then e:AddComponent("follower");f=e.components.follower end;if f and(f.leader==nil or not f.leader:HasTag("bell"))then if e.components.combat and e.components.combat:TargetIs(p)then e.components.combat:SetTarget(nil)end;if f.leader~=p then f:SetLeader(p);p:PushEvent("makefriend");p.components.leader:AddFollower(e);if not clockwork then f:AddLoyaltyTime(6000);f.maxfollowtime=math.max(f.maxfollowtime or 0,6000)end;n=n+1 end end end`,
		"dismiss": `local f=e.components and e.components.follower;if f and p.components and p.components.leader and p.components.leader:IsFollower(e)then p.components.leader:RemoveFollower(e);f.targettime=0;n=n+1 end`,
		"heal":    `local f=e.components and e.components.follower;if f and p.components and p.components.leader and p.components.leader:IsFollower(e)and e.components.health then e.components.health:SetPercent(1);n=n+1 end`,
		"feed":    `local f=e.components and e.components.follower;if f and p.components and p.components.leader and p.components.leader:IsFollower(e)and e.components.hunger then e.components.hunger:SetPercent(1);n=n+1 end`,
		"loyal":   `local f=e.components and e.components.follower;if f and p.components and p.components.leader and p.components.leader:IsFollower(e)then f.targettime=(f.maxfollowtime or 6000)+GetTime();if e.components.domesticatable then e.components.domesticatable:DeltaObedience(1)end;n=n+1 end`,
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();local n=0;for _,e in pairs(TheSim:FindEntities(x,y,z,%d,nil,{"INLIMBO","playerghost"}))do if e:IsValid()then %s end end;print("[DST-ADMIN-FOLLOWERS]",%s,%s,n)else print("[DST-ADMIN-FOLLOWERS]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), radius, actions[action], quoteLua(playerID), quoteLua(action), quoteLua(playerID)), nil
}

func renderSpawnDomesticatedBeefalo(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	tendency, err := enumArgument(arguments, "tendency", "default", "ornery", "pudgy", "rider", "custom")
	if err != nil {
		return "", err
	}
	tendencies := map[string]string{"default": "TENDENCY.DEFAULT", "ornery": "TENDENCY.ORNERY", "pudgy": "TENDENCY.PUDGY", "rider": "TENDENCY.RIDER"}
	saddles := map[string]string{"default": "saddle_shadow", "ornery": "saddle_war", "pudgy": "saddle_basic", "rider": "saddle_race"}
	if tendency == "custom" {
		saddle, err := optionalEnumArgument(arguments, "saddle", "none", "none", "shadow", "basic", "war", "race", "wathgrithr")
		if err != nil {
			return "", err
		}
		bell, err := optionalEnumArgument(arguments, "bell", "none", "none", "beef_bell", "shadow_beef_bell")
		if err != nil {
			return "", err
		}
		values := make(map[string]float64, 7)
		for _, input := range []struct {
			name     string
			fallback float64
		}{
			{"domestication", 100}, {"hunger", 50}, {"obedience", 100}, {"health", 100},
			{"ornery", 0}, {"rider", 0}, {"pudgy", 0},
		} {
			values[input.name], err = optionalFloatArgument(arguments, input.name, input.fallback, 0, 100)
			if err != nil {
				return "", err
			}
		}
		saddlePrefab := map[string]string{"none": "", "shadow": "saddle_shadow", "basic": "saddle_basic", "war": "saddle_war", "race": "saddle_race", "wathgrithr": "saddle_wathgrithr"}[saddle]
		return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();local b=SpawnPrefab("beefalo");local c=b and b.components;if b and c and c.domesticatable and c.hunger and c.health and c.rideable then b.Transform:SetPosition(x,y,z);c.domesticatable.tendencies={ORNERY=%s,RIDER=%s,PUDGY=%s};c.domesticatable.domestication=%s;c.domesticatable.obedience=%s;c.hunger:SetPercent(%s);c.health:SetPercent(%s);b:SetTendency();if %s>=1 then c.domesticatable:BecomeDomesticated()end;local saddle_prefab=%s;if saddle_prefab~=""then local saddle=SpawnPrefab(saddle_prefab);if saddle then c.rideable:SetSaddle(nil,saddle)end end;local bell_prefab=%s;if bell_prefab~=""and p.components and p.components.inventory then local bell=SpawnPrefab(bell_prefab);if bell then p.components.inventory:GiveItem(bell);if bell.components and bell.components.useabletargeteditem then bell.components.useabletargeteditem:StartUsingItem(b,p)end end end;print("[DST-ADMIN-BEEFALO]",%s,"custom",true)else if b then b:Remove()end;print("[DST-ADMIN-BEEFALO]",%s,"custom",false)end else print("[DST-ADMIN-BEEFALO]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), luaNumber(values["ornery"]), luaNumber(values["rider"]), luaNumber(values["pudgy"]), luaNumber(values["domestication"]/100), luaNumber(values["obedience"]/100), luaNumber(values["hunger"]/100), luaNumber(values["health"]/100), luaNumber(values["domestication"]/100), quoteLua(saddlePrefab), quoteLua(map[string]string{"none": "", "beef_bell": "beef_bell", "shadow_beef_bell": "shadow_beef_bell"}[bell]), quoteLua(playerID), quoteLua(playerID), quoteLua(playerID)), nil
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();local b=SpawnPrefab("beefalo");local c=b and b.components;if b and c and c.domesticatable and c.hunger and c.rideable then b.Transform:SetPosition(x,y,z);c.domesticatable:DeltaDomestication(1);c.domesticatable:DeltaObedience(1);c.domesticatable:DeltaTendency(%s,1);b:SetTendency();c.domesticatable:BecomeDomesticated();c.hunger:SetPercent(.5);local saddle=SpawnPrefab(%s);if saddle then c.rideable:SetSaddle(nil,saddle)end;print("[DST-ADMIN-BEEFALO]",%s,%s,true)else if b then b:Remove()end;print("[DST-ADMIN-BEEFALO]",%s,%s,false)end else print("[DST-ADMIN-BEEFALO]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), tendencies[tendency], quoteLua(saddles[tendency]), quoteLua(playerID), quoteLua(tendency), quoteLua(playerID), quoteLua(tendency), quoteLua(playerID)), nil
}

func renderSetPrecipitation(arguments map[string]interface{}) (string, error) {
	mode, err := enumArgument(arguments, "mode", "start", "stop", "dynamic")
	if err != nil {
		return "", err
	}
	scripts := map[string]string{
		"start":   `TheWorld:PushEvent("ms_setprecipitationmode","always");TheWorld:PushEvent("ms_forceprecipitation",true)`,
		"stop":    `TheWorld:PushEvent("ms_setprecipitationmode","never");TheWorld:PushEvent("ms_forceprecipitation",false)`,
		"dynamic": `TheWorld:PushEvent("ms_forceprecipitation",false);TheWorld:PushEvent("ms_setprecipitationmode","dynamic")`,
	}
	return scripts[mode] + `;print("[DST-ADMIN-PRECIPITATION]",` + quoteLua(mode) + `)`, nil
}

func renderTriggerWorldEvent(arguments map[string]interface{}) (string, error) {
	event, err := enumArgument(arguments, "event", "lightning", "earthquake", "lunar_hail", "meteor_shower", "acid_rain", "moon_storm", "nightmare_calm", "nightmare_warn", "nightmare_wild", "nightmare_dawn", "reset_ruins")
	if err != nil {
		return "", err
	}
	switch event {
	case "earthquake":
		return `TheWorld:PushEvent("ms_forcequake");print("[DST-ADMIN-WORLD-EVENT]","earthquake")`, nil
	case "lunar_hail":
		return `if TheWorld.state.islunarhailing then local w=TheWorld.net and TheWorld.net.components.weather;if w then w:LongUpdate(TUNING.LUNARHAIL_EVENT_TIME)end else TheWorld:PushEvent("ms_startlunarhail")end;print("[DST-ADMIN-WORLD-EVENT]","lunar_hail")`, nil
	case "acid_rain":
		return `local raining=TheWorld.state.isacidraining or TheWorld.state.precipitation=="acidrain";if raining then TheWorld:PushEvent("ms_setprecipitationmode","never");TheWorld:PushEvent("ms_forceprecipitation",false);TheWorld:DoTaskInTime(1,function(w)w:PushEvent("ms_setprecipitationmode","dynamic")end)else local r=TheWorld.components and TheWorld.components.riftspawner;local old=TUNING.ACIDRAIN_ENABLED;TUNING.ACIDRAIN_ENABLED=true;local fn=r and r.IsShadowPortalActive;if r and fn then r.IsShadowPortalActive=function()return true end end;TheWorld:PushEvent("ms_setprecipitationmode","dynamic");TheWorld:PushEvent("ms_forceprecipitation",true);TheWorld:DoTaskInTime(2,function()if r and fn then r.IsShadowPortalActive=fn end;TUNING.ACIDRAIN_ENABLED=old end)end;print("[DST-ADMIN-WORLD-EVENT]","acid_rain")`, nil
	case "moon_storm":
		return `local c=TheWorld.net and TheWorld.net.components.moonstorms;local active=c and next(c:GetMoonstormNodes())~=nil;if active then TheWorld:PushEvent("ms_stopthemoonstorms")else TheWorld:PushEvent("ms_startthemoonstorms")end;print("[DST-ADMIN-WORLD-EVENT]","moon_storm",not active)`, nil
	case "nightmare_calm", "nightmare_warn", "nightmare_wild", "nightmare_dawn":
		phase := map[string]string{"nightmare_calm": "calm", "nightmare_warn": "warn", "nightmare_wild": "wild", "nightmare_dawn": "dawn"}[event]
		return `TheWorld:PushEvent("nightmarephasechanged",` + quoteLua(phase) + `);print("[DST-ADMIN-WORLD-EVENT]",` + quoteLua(event) + `)`, nil
	case "reset_ruins":
		return `TheWorld:PushEvent("resetruins");print("[DST-ADMIN-WORLD-EVENT]","reset_ruins")`, nil
	case "lightning", "meteor_shower":
		playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
		if err != nil {
			return "", err
		}
		if event == "meteor_shower" {
			return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local x,y,z=p.Transform:GetWorldPosition();local spawned=0;local task=nil;local function emit()local a=math.random()*TWOPI;local r=math.random()*12;local tx,tz=x+r*math.cos(a),z-r*math.sin(a);if TheWorld.Map:IsPassableAtPoint(tx,y,tz)then local e=SpawnPrefab("shadowmeteor");if e then e.Transform:SetPosition(tx,y,tz);local n=math.random();e:SetSize(n<=.15 and "large"or(n<=.45 and "medium"or"small"),1)end end;spawned=spawned+1;if spawned>=8 and task then task:Cancel()end end;task=p:DoPeriodicTask(.3,emit);emit();print("[DST-ADMIN-WORLD-EVENT]","meteor_shower",%s)else print("[DST-ADMIN-WORLD-EVENT]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), quoteLua(playerID), quoteLua(playerID)), nil
		}
		return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then TheWorld:PushEvent("ms_sendlightningstrike",Vector3(p.Transform:GetWorldPosition()));print("[DST-ADMIN-WORLD-EVENT]","lightning",%s)else print("[DST-ADMIN-WORLD-EVENT]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), quoteLua(playerID), quoteLua(playerID)), nil
	}
	return "", ErrInvalidArguments
}

func renderSetSpecialEvent(arguments map[string]interface{}) (string, error) {
	event, err := enumArgument(arguments, "event", "none", "default", "crow_carnival", "hallowed_nights", "winters_feast", "year_of_the_gobbler", "year_of_the_varg", "year_of_the_pig", "year_of_the_carrat", "year_of_the_beefalo", "year_of_the_catcoon", "year_of_the_bunnyman", "year_of_the_dragonfly", "year_of_the_snake", "year_of_the_knight")
	if err != nil {
		return "", err
	}
	return `local event=` + quoteLua(event) + `;ApplySpecialEvent(event);TheWorld.topology.overrides.specialevent=event;c_save();TheWorld:DoTaskInTime(5,function()if TheWorld and TheWorld.ismastersim then TheNet:SendWorldRollbackRequestToServer(0)end end);print("[DST-ADMIN-SPECIAL-EVENT]",event)`, nil
}

func renderTriggerIncident(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	incident, err := enumArgument(arguments, "incident", "hounds", "worms", "worm_boss", "frog_rain", "brightshade", "pirates")
	if err != nil {
		return "", err
	}
	actions := map[string]string{
		"hounds":      `local h=TheWorld.components and TheWorld.components.hounded;if h then if h.ForceNextWave then h:ForceNextWave()end;if h.ForceReleaseSpawn then for i=1,3 do h:ForceReleaseSpawn(p)end end;success=true end`,
		"worms":       `local h=TheWorld.components and TheWorld.components.hounded;if h then if h.ForceNextWave then h:ForceNextWave()end;if h.ForceReleaseSpawn then for i=1,3 do h:ForceReleaseSpawn(p)end end;success=true end`,
		"worm_boss":   `local elapsed=0;local function warn()elapsed=elapsed+1.5;if elapsed>=10 then local worm=SpawnPrefab("worm_boss");if worm then local pt=p:GetPosition();local off=FindWalkableOffset(pt,math.random()*TWOPI,18,16,nil,true);local pos=off and(pt+off)or pt;if worm.Physics then worm.Physics:Teleport(pos:Get())else worm.Transform:SetPosition(pos:Get())end;worm:FacePoint(pt);if worm.components.spawnfader then worm.components.spawnfader:FadeIn()end;if worm.components.combat then worm.components.combat:SuggestTarget(p)end end;return end;TheWorld:PushEvent("ms_miniquake",{rad=20,num=20,duration=1.5,target=p});if HOUNDWARNINGTYPE then p:PushEvent("houndwarning",HOUNDWARNINGTYPE.WORM_BOSS)end;p:DoTaskInTime(1.5,warn)end;warn();success=true`,
		"frog_rain":   `local f=TheWorld.components and TheWorld.components.frograin;if f then TheWorld:PushEvent("ms_forceprecipitation",true);local cap=math.random(TUNING.FROG_RAIN_LOCAL_MIN,TUNING.FROG_RAIN_LOCAL_MAX);local spawned=0;local function emit()if spawned>=cap then return end;local pt=p:GetPosition();local theta=math.random()*TWOPI;local radius=math.random()*TUNING.FROG_RAIN_SPAWN_RADIUS;local off=FindValidPositionByFan(theta,radius,12,function(o)local pos=pt+o;return TheWorld.Map:IsAboveGroundAtPoint(pos:Get())end);if off then local frog=SpawnPrefab("frog");frog.persists=false;frog.sg:GoToState("fall");local pos=pt+off;frog.Physics:Teleport(pos.x,35,pos.z);f:StartTracking(frog);spawned=spawned+1 end;TheWorld:DoTaskInTime(GetRandomMinMax(TUNING.FROG_RAIN_DELAY.min,TUNING.FROG_RAIN_DELAY.max),emit)end;TheWorld:DoTaskInTime(1,emit);success=true end`,
		"brightshade": `local s=TheWorld.components and TheWorld.components.lunarthrall_plantspawner;if s then local x,y,z=p.Transform:GetWorldPosition();local plants={};for _,e in ipairs(TheSim:FindEntities(x,y,z,40,{"plant","lunarplant_target"},{"INLIMBO"}))do if not e.lunarthrall_plant and(not e.components.witherable or not e.components.witherable:IsWithered())and not s.targetedplants[e.GUID]then plants[#plants+1]=e end end;success=#plants>0;for i=1,math.min(3,#plants)do local target=table.remove(plants,math.random(#plants));s.targetedplants[target.GUID]=true;local pos=target:GetPosition();local angle=math.random()*TWOPI;local e=SpawnPrefab("lunarthrall_plant_gestalt");e.plant_target=target;e.Transform:SetPosition(pos.x+math.cos(angle)*12,pos.y,pos.z-math.sin(angle)*12);e:Spawn()end end`,
		"pirates":     `local s=TheWorld.components and TheWorld.components.piratespawner;if s and p:GetCurrentPlatform()then s:SpawnPiratesForPlayer(p);success=true end`,
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;local success=false;if p and p.Transform then %s end;print("[DST-ADMIN-INCIDENT]",%s,%s,success)`, quoteLua(playerID), actions[incident], quoteLua(playerID), quoteLua(incident)), nil
}

func renderNearbyEntityAction(arguments map[string]interface{}) (string, error) {
	playerID, err := matchedArgument(arguments, "player_id", dstUserIDPattern)
	if err != nil {
		return "", err
	}
	prefab, err := optionalMatchedArgument(arguments, "prefab", prefabPattern)
	if err != nil {
		return "", err
	}
	action, err := enumArgument(arguments, "action", "delete", "extinguish", "ignite", "restore", "repair", "repair_boat", "freshen", "heat", "cool", "salvage", "haunt", "fertilize", "grow", "harvest", "pick", "chop", "mine", "hammer", "dig", "till", "build_complete", "chaos", "electrocute", "kill", "freeze", "sleep", "panic", "pacify", "root", "taunt")
	if err != nil {
		return "", err
	}
	radius, err := intArgument(arguments, "radius", 1, 64)
	if err != nil {
		return "", err
	}
	layout, err := optionalEnumArgument(arguments, "layout", "3x3", "2x2", "3x3", "4x4", "hexagon")
	if err != nil {
		return "", err
	}
	actions := map[string]string{
		"delete":         `local protected={FX=true,NOCLICK=true,DECOR=true,CLASSIFIED=true,placer=true};for _,e in pairs(ents)do local inv=e.components and e.components.inventoryitem;local blocked=false;for tag in pairs(protected)do if e:HasTag(tag)then blocked=true;break end end;if matches(e)and e~=p and e~=TheWorld and e.prefab and e.entity:GetParent()==nil and not(inv and inv.owner)and not blocked and(not e:HasTag("player")or not e.userid)then if e.components and e.components.burnable then e.components.burnable:Extinguish(true)end;e:Remove();n=n+1 end end`,
		"extinguish":     `local excluded={campfire=true,firepit=true,coldfire=true,coldfirepit=true,nightlight=true};for _,e in pairs(ents)do if matches(e)and e~=p and not excluded[e.prefab]and e.components then local changed=false;if e.components.burnable and e.components.burnable:IsBurning()then e.components.burnable:Extinguish(true);changed=true end;if e.components.firefx then e.components.firefx:Extinguish();changed=true end;if changed then n=n+1 end end end`,
		"ignite":         `for _,e in pairs(ents)do local inv=e.components and e.components.inventoryitem;if matches(e)and e~=p and e.components and e.components.burnable and not(inv and(inv.owner or inv:IsHeld()))and not e.components.burnable:IsBurning()then e.components.burnable:Ignite(nil,nil,p);n=n+1 end end`,
		"restore":        `local targets={};for _,e in pairs(ents)do if matches(e)and e:HasTag("burnt")and e:HasTag("structure")then targets[#targets+1]=e end end;for _,e in ipairs(targets)do local pos=e:GetPosition();local name,skin=e.prefab,e.skinname;e:Remove();local restored=SpawnPrefab(name,skin,nil,p.userid);if restored then restored.Transform:SetPosition(pos:Get());n=n+1 end end`,
		"repair":         `local seen={};local function repair(e)if not e or seen[e]or not e:IsValid()then return end;seen[e]=true;if matches(e)and e.components then local changed=false;for _,name in ipairs({"finiteuses","armor","fueled"})do local c=e.components[name];if c and c.SetPercent and(c.GetPercent==nil or c:GetPercent()<1)then c:SetPercent(1);changed=true end end;if changed then n=n+1 end end;local c=e.components and e.components.container;if c then for _,item in pairs(c.slots or {})do repair(item)end end end;for _,e in pairs(ents)do repair(e)end;visitplayeritems(repair)`,
		"repair_boat":    `local function leak(e)local c=e.components and e.components.boatleak;if not c then return false end;if c.SetState then c:SetState("repaired_treegrowth")elseif c.ChangeToRepaired then c:ChangeToRepaired("treegrowthsolution","waterlogged2/common/repairgoop")else e:Remove()end;return true end;for _,e in pairs(ents)do if matches(e)then if leak(e)then n=n+1 else local c=e.components or {};if e:HasTag("boat")or e:HasTag("walkableplatform")or c.hullhealth then local changed=false;if c.health and not c.health:IsDead()and(c.health.GetPercent==nil or c.health:GetPercent()<1)then c.health:SetPercent(1);changed=true end;if c.hullhealth then for _,list in ipairs({c.hullhealth.leak_indicators,c.hullhealth.leak_indicators_dynamic})do for key,item in pairs(list or {})do if leak(item)then changed=true end;list[key]=nil end end;e:RemoveTag("is_leaking")end;if changed then n=n+1 end end end end end`,
		"freshen":        `local seen={};local function fresh(e)if not e or seen[e]or not e:IsValid()then return end;seen[e]=true;if matches(e)and e.components and e.components.perishable then local c=e.components.perishable;if c.GetPercent==nil or c:GetPercent()<1 then c:SetPercent(1);n=n+1 end end;local c=e.components and e.components.container;if c then for _,item in pairs(c.slots or {})do fresh(item)end end end;for _,e in pairs(ents)do fresh(e)end;visitplayeritems(fresh)`,
		"heat":           `local seen={};local function temp(e)if not e or seen[e]or not e:IsValid()then return end;seen[e]=true;if matches(e)and not e:HasTag("player")and e.components then local changed=false;if e.components.temperature then e.components.temperature:SetTemperature(math.max(e.components.temperature:GetCurrent(),70));changed=true end;if e.components.inventoryitemtemperature and e.components.inventoryitem then e.components.inventoryitemtemperature:SetTemperature(math.max(e.components.inventoryitem:GetTemperature(),70));changed=true end;if changed then n=n+1 end end;local c=e.components and e.components.container;if c then for _,item in pairs(c.slots or {})do temp(item)end end end;for _,e in pairs(ents)do temp(e)end;visitplayeritems(temp)`,
		"cool":           `local seen={};local function temp(e)if not e or seen[e]or not e:IsValid()then return end;seen[e]=true;if matches(e)and not e:HasTag("player")and e.components then local changed=false;if e.components.temperature then e.components.temperature:SetTemperature(math.min(e.components.temperature:GetCurrent(),-20));changed=true end;if e.components.inventoryitemtemperature and e.components.inventoryitem then e.components.inventoryitemtemperature:SetTemperature(math.min(e.components.inventoryitem:GetTemperature(),-20));changed=true end;if changed then n=n+1 end end;local c=e.components and e.components.container;if c then for _,item in pairs(c.slots or {})do temp(item)end end end;for _,e in pairs(ents)do temp(e)end;visitplayeritems(temp)`,
		"salvage":        `for _,e in pairs(ents)do if matches(e)and e.components and e.components.winchtarget then local item=e.components.winchtarget:Salvage();if item and p.components.inventory then p.components.inventory:GiveItem(item);item:PushEvent("on_salvaged")end;e:Remove();n=n+1 end end`,
		"haunt":          `for _,e in pairs(ents)do if matches(e)and e~=p and e.components and e.components.hauntable then e.components.hauntable:DoHaunt(p);n=n+1 end end`,
		"fertilize":      `for _,e in pairs(ents)do if matches(e)and e.components then local f=nil;if e.components.crop and not e.components.crop:IsReadyForHarvest()and not e:HasTag("withered")then f=SpawnPrefab("compostwrap");e.components.crop:Fertilize(f)elseif e.components.grower and e.components.grower:IsEmpty()then f=SpawnPrefab("compostwrap");e.components.grower:Fertilize(f)elseif e.components.pickable and e.components.pickable:CanBeFertilized()then f=SpawnPrefab("compostwrap");e.components.pickable:Fertilize(f)end;if f then f:Remove();n=n+1 end end end`,
		"grow":           `for _,e in pairs(ents)do if matches(e)and e.components then local c=e.components;local changed=false;if c.growable then if c.growable.DoMagicGrowth then c.growable:DoMagicGrowth()else c.growable:DoGrowth()end;changed=true elseif c.crop and not c.crop:IsReadyForHarvest()then c.crop:DoGrow(1,true);changed=true elseif c.pickable and not c.pickable:CanBePicked()and c.pickable.FinishGrowing then c.pickable:FinishGrowing();changed=true elseif c.harvestable and c.harvestable.Grow then c.harvestable:Grow();changed=true elseif c.timer then for name in pairs(c.timer.timers or {})do c.timer:SetTimeLeft(name,0);changed=true end end;if changed then n=n+1 end end end`,
		"harvest":        `for _,e in pairs(ents)do if matches(e)and e~=TheWorld and not e:HasTag("player")and not e:HasTag("flower")and not e:HasTag("trap")and e.components then local c=e.components;local changed=false;if c.crop and c.crop:IsReadyForHarvest()then c.crop:Harvest(p);changed=true elseif c.harvestable and c.harvestable:CanBeHarvested()then c.harvestable:Harvest(p);changed=true elseif c.stewer and c.stewer:IsDone()then c.stewer:Harvest(p);changed=true elseif c.dryer and c.dryer:IsDone()then c.dryer:Harvest(p);changed=true elseif c.occupiable and c.occupiable:IsOccupied()then local item=c.occupiable:Harvest(p);if item and p.components.inventory then p.components.inventory:GiveItem(item);changed=true end elseif c.pickable and c.pickable:CanBePicked()then c.pickable:Pick(p);changed=true end;if changed then n=n+1 end end end`,
		"pick":           `local inv=p.components and p.components.inventory;for _,e in pairs(ents)do local item=e.components and e.components.inventoryitem;if matches(e)and inv and item and item.canbepickedup and item.cangoincontainer and not item:IsHeld()and not e:HasTag("flower")and not e:HasTag("trap")and not e:HasTag("mine")and inv:CanAcceptCount(e,1)>0 then inv:GiveItem(e);n=n+1 end end`,
		"chop":           workActionScript("CHOP", false),
		"mine":           workActionScript("MINE", true),
		"hammer":         workActionScript("HAMMER", true),
		"dig":            workActionScript("DIG", false),
		"till":           `local map=TheWorld.Map;local scale=TILE_SCALE or 4;local tr=math.max(0,math.ceil(R/scale));local tcx,tcy=map:GetTileCoordsAtPoint(x,0,z);local function offsets(tilex)if layout=="hexagon"then local s=1.6;local h=s*.5;local counts=(math.floor(tilex/2)*2~=tilex)and{3,2,3,2}or{2,3,2,3};local result={};for row,count in ipairs(counts)do local zo=(row-2.5);if count==2 then result[#result+1]={-h,zo};result[#result+1]={h,zo}else result[#result+1]={-s,zo};result[#result+1]={0,zo};result[#result+1]={s,zo}end end;return result end;local size=tonumber(string.match(layout,"^(%d+)x%d+$"))or 3;local half=(size-1)*.5;local result={};for ix=0,size-1 do for iz=0,size-1 do result[#result+1]={(ix-half)*4/3,(iz-half)*4/3}end end;return result end;for tx=tcx-tr,tcx+tr do for ty=tcy-tr,tcy+tr do local cx,_,cz=map:GetTileCenterPoint(tx,ty);local dx,dz=cx-x,cz-z;if dx*dx+dz*dz<=R*R and map:IsFarmableSoilAtPoint(cx,0,cz)then local points=offsets(tx);for _,soil in ipairs(map:GetEntitiesOnTileAtPoint(cx,0,cz))do if soil:IsValid()and soil:HasTag("soil")and not soil:HasTag("NOBLOCK")then local sx,_,sz=soil.Transform:GetWorldPosition();local keep=false;for _,o in ipairs(points)do local ox,oz=sx-(cx+o[1]),sz-(cz+o[2]);if ox*ox+oz*oz<.04 then keep=true;break end end;if not keep then soil:Remove();n=n+1 end end end;for _,o in ipairs(points)do local px,pz=cx+o[1],cz+o[2];if #TheSim:FindEntities(px,0,pz,.21,{"soil"},{"merm_soil_blocker","farm_debris","NOBLOCK"})<1 and map:CanTillSoilAtPoint(px,0,pz,false)then local soil=SpawnPrefab("farm_soil");if soil then soil.Transform:SetPosition(px,0,pz);n=n+1 end end end end end end;if n>0 then p:PushEvent("tilling")end`,
		"build_complete": `for _,e in pairs(ents)do local c=e.components and e.components.constructionsite;if matches(e)and c and not c:IsComplete()then c:ForceCompletion(p);n=n+1 end end`,
		"chaos":          `for _,e in pairs(ents)do if matches(e)and e~=TheWorld and not e:HasTag("player")and e.components and e.components.combat and e.components.health and not e.components.health:IsDead()then local candidates={};local ex,ey,ez=e.Transform:GetWorldPosition();for _,v in pairs(TheSim:FindEntities(ex,ey,ez,20,nil,{"INLIMBO","FX","player","playerghost"}))do if v~=e and v~=TheWorld and v.components and v.components.health and not v.components.health:IsDead()then candidates[#candidates+1]=v end end;if #candidates>0 then e.components.combat:SetTarget(candidates[math.random(#candidates)]);n=n+1 end end end`,
		"electrocute":    `for _,e in pairs(ents)do if matches(e)and e~=TheWorld and not e:HasTag("player")and e.components and e.components.combat then SpawnElectricHitSparks(p,e,true);e:PushEventImmediate("electrocute");n=n+1 end end`,
		"kill":           `for _,e in pairs(ents)do local c=e.components or {};local ridden=c.rideable and c.rideable:IsBeingRidden();local owned=e:HasTag("structure")or e:HasTag("wall")or e:HasTag("boat")or e:HasTag("playerowned")or(c.follower and c.follower:GetLeader()~=nil)or(c.domesticatable and c.domesticatable:IsDomesticated());if matches(e)and e~=TheWorld and not e:HasTag("player")and not ridden and not owned and c.health and not c.health:IsDead()then c.health:Kill();n=n+1 end end`,
		"freeze":         `for _,e in pairs(ents)do if matches(e)and not e:HasTag("player")and e.components and e.components.freezable then e.components.freezable:AddColdness(1,60);e.components.freezable:SpawnShatterFX();n=n+1 end end`,
		"sleep":          `for _,e in pairs(ents)do if matches(e)and not e:HasTag("player")and e.components and e.components.sleeper then e.components.sleeper:AddSleepiness(10,20);n=n+1 end end`,
		"panic":          `for _,e in pairs(ents)do local h=e.components and e.components.hauntable;if matches(e)and not e:HasTag("player")and h and h.panicable then h:Panic(60);n=n+1 end end`,
		"pacify":         `for _,e in pairs(ents)do local c=e.components or {};if matches(e)and c.combat and c.combat.target and c.combat.target:HasTag("player")then c.combat:SetTarget(nil);c.combat:GiveUp();n=n+1 end;if matches(e)and e:HasTag("leif")and c.sleeper then c.sleeper:GoToSleep(1000)end end`,
		"root":           `for _,e in pairs(ents)do local c=e.components or {};if matches(e)and e~=p and not e:HasTag("player")and c.locomotor and c.health and not c.health:IsDead()then if not c.rooted then e:AddComponent("rooted")end;c.rooted:AddSource(p);e:DoTaskInTime(60,function(inst)if inst and inst.components.rooted then inst.components.rooted:RemoveSource(p)end end);n=n+1 end end`,
		"taunt":          `for _,e in pairs(ents)do local c=e.components or {};if matches(e)and e~=TheWorld and not e:HasTag("player")and not e:HasTag("bird")and c.combat then c.combat:SetTarget(p);e:DoTaskInTime(60,function(inst)if inst and inst.components.combat and inst.components.combat.target==p then inst.components.combat:SetTarget(nil);inst.components.combat:GiveUp()end end);n=n+1 end end`,
	}
	return fmt.Sprintf(`local p=nil;for _,v in ipairs(AllPlayers or {})do if v.userid==%s then p=v;break end end;if p and p.Transform then local R=%d;local filter=%s;local layout=%s;local function matches(e)return filter==""or e.prefab==filter end;local x,y,z=p.Transform:GetWorldPosition();local ents=TheSim:FindEntities(x,y,z,R,nil,{"INLIMBO"});local n=0;local function visitplayeritems(fn)for _,target in ipairs(AllPlayers or {p})do if target.Transform and target:GetDistanceSqToPoint(x,y,z)<=R*R then local inv=target.components and target.components.inventory;if inv then for _,item in pairs(inv.itemslots or {})do fn(item)end;for _,item in pairs(inv.equipslots or {})do fn(item)end;fn(inv.activeitem);if inv.overflow then fn(inv.overflow.inst)end end end end end;%s;print("[DST-ADMIN-ENTITY-ACTION]",%s,filter,%s,n)else print("[DST-ADMIN-ENTITY-ACTION]","PLAYER_NOT_FOUND",%s)end`, quoteLua(playerID), radius, quoteLua(prefab), quoteLua(layout), actions[action], quoteLua(playerID), quoteLua(action), quoteLua(playerID)), nil
}

func workActionScript(action string, strong bool) string {
	tough := "false"
	if strong {
		tough = "true"
	}
	return `local action=ACTIONS.` + action + `;local had=p:HasTag("toughworker");if ` + tough + ` and not had then p:AddTag("toughworker")end;for _,e in pairs(ents)do local w=e.components and e.components.workable;if matches(e)and w and w:CanBeWorked()and w:GetWorkAction()==action then w:WorkedBy(p,w.workleft or 1);n=n+1 end end;if ` + tough + ` and not had then p:RemoveTag("toughworker")end`
}

func optionalMatchedArgument(arguments map[string]interface{}, name string, pattern interface{ MatchString(string) bool }) (string, error) {
	value, exists := arguments[name]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", ErrInvalidArguments
	}
	if text == "" {
		return "", nil
	}
	if !pattern.MatchString(text) {
		return "", ErrInvalidArguments
	}
	return text, nil
}

func optionalEnumArgument(arguments map[string]interface{}, name, fallback string, options ...string) (string, error) {
	value, exists := arguments[name]
	if !exists || value == nil || value == "" {
		return fallback, nil
	}
	return enumArgument(arguments, name, options...)
}

func boolArgument(arguments map[string]interface{}, name string) (bool, error) {
	value, exists := arguments[name]
	if !exists {
		return false, ErrInvalidArguments
	}
	result, ok := value.(bool)
	if !ok {
		return false, ErrInvalidArguments
	}
	return result, nil
}

func floatArgument(arguments map[string]interface{}, name string, minimum, maximum float64) (float64, error) {
	var value float64
	switch typed := arguments[name].(type) {
	case float64:
		value = typed
	case int:
		value = float64(typed)
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, ErrInvalidArguments
		}
		value = parsed
	default:
		return 0, ErrInvalidArguments
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < minimum || value > maximum {
		return 0, ErrInvalidArguments
	}
	return value, nil
}

func optionalFloatArgument(arguments map[string]interface{}, name string, fallback, minimum, maximum float64) (float64, error) {
	raw, exists := arguments[name]
	if !exists || raw == nil || raw == "" {
		return fallback, nil
	}
	return floatArgument(arguments, name, minimum, maximum)
}

func luaNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
