package console

import "strings"

// Action is presentation data for a code-owned enum, never an executable recipe.
// Parameters contain suggestions; every execution still validates the original
// command and arguments. Risk cannot be supplied by the caller.
type Action struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	SearchTerms []string          `json:"searchTerms"`
	Parameters  map[string]string `json:"parameters"`
	Risk        Risk              `json:"risk"`
	Description string            `json:"description,omitempty"`
	Interaction *Interaction      `json:"interaction,omitempty"`
}

// Interaction is a finite client protocol, not a recipe or an authorization.
type Interaction struct {
	Kind          string `json:"kind"`
	ResultVersion int    `json:"resultVersion"`
	EffectVersion string `json:"effectVersion"`
}

func actionInteraction(command, action string) *Interaction {
	if command == "act_nearby_entities" && guardedNearby(action) {
		version := 1
		if nearbyV2(action) {
			version = 2
		}
		return &Interaction{Kind: "preview-confirm-v1", ResultVersion: version, EffectVersion: "1"}
	}
	if guardedExpansion(command) {
		return &Interaction{Kind: "preview-confirm-v1", ResultVersion: 2, EffectVersion: "1"}
	}
	if command == "set_player_hunger_rate" || command == "set_wanda_age" || command == "restore_woodie_human" {
		return &Interaction{Kind: "preview-confirm-v1", ResultVersion: 2, EffectVersion: "1"}
	}
	return nil
}

func actionRisk(command, action string, fallback Risk) Risk {
	if command == "act_nearby_entities" && nearbyV2(action) {
		if action == "repair_burnt" {
			return RiskCritical
		}
		return RiskHigh
	}
	return fallback
}

// RequiresHumanConfirmation gates model transports as well as the fixed API.
// Preparation/cancellation is allowed only for our code-owned interactions.
func RequiresHumanConfirmation(id string, args map[string]interface{}) bool {
	action, _ := args["action"].(string)
	return actionInteraction(id, action) != nil && nearbyPhase(args) == "execute"
}

// WorkbenchBehaviorRevision invalidates unsubmitted tasks when fixed command
// semantics change without changing their public parameter schema.
const WorkbenchBehaviorRevision = "7"

func commandActions(d Definition) []Action {
	field := map[string]string{"trigger_world_event": "event", "set_player_ability": "ability", "set_character_power": "action", "act_nearby_entities": "action", "set_player_lock": "resource", "set_player_mode": "mode", "set_daylight_mode": "mode", "spawn_special_structure": "kind", "spawn_entity_array": "layout", "set_wurt_form": "form", "apply_all_players": "action"}[d.ID]
	if field == "" {
		return nil
	}
	var out []Action
	for _, p := range d.Parameters {
		if p.Name != field {
			continue
		}
		for _, value := range p.Options {
			words := actionWords[d.ID][value]
			if words == "" {
				continue
			}
			terms := strings.Fields(words)
			out = append(out, Action{ID: d.ID + "." + value, Title: terms[0], SearchTerms: terms, Parameters: map[string]string{field: value}, Risk: actionRisk(d.ID, value, d.Risk), Description: actionDescription(d.ID, value), Interaction: actionInteraction(d.ID, value)})
		}
	}
	return out
}

func actionDescription(command, action string) string {
	if command == "act_nearby_entities" {
		switch action {
		case "repair":
			return "只修复地面装备的使用次数和护甲耐久；不补燃料、不充能、不处理背包或容器。"
		case "repair_burnt":
			return "只重建原版科学机器和炼金引擎，保留位置、朝向和可用皮肤；不能找回已丢失的数据。"
		case "pick":
			return "拾取地面无持有者的物品，交给中心玩家的主物品栏；不处理容器内部，空间不足时保留原物。"
		case "fertilize":
			return "为原生组件判断需要施肥的草、树苗、普通浆果丛等支持植物补充肥料；不扣玩家材料，不代表恢复农田营养。"
		case "grow":
			return "支持的原版农作物推进一个阶段，草、树苗、普通浆果丛和芦苇使用原生再生方法；不清零其他计时器。"
		case "ripen":
			return "有界推进支持的原版植物至可收获状态，并核对结果；不保证巨大化。"
		case "harvest":
			return "采摘支持的草、树苗、普通浆果丛和芦苇，产物交给中心玩家；随机农作物掉落、锅和晾肉架不在首批范围。"
		}
	}
	return actionDescriptions[command]
}

var actionDescriptions = map[string]string{
	"act_nearby_entities": "仅作用于所选世界和范围；需要确认。调用成功不等于所有对象效果均已验证。",
	"set_player_ability":  "必须明确开启或关闭；取决于当前角色和组件。关闭不会撤销已经发生的效果。",
	"set_character_power": "需要对应角色或随从存在于所选世界；不支持时不会替换为其他动作。",
	"trigger_world_event": "改变所选世界；闪电和流星雨还需指定定位玩家。",
	"set_player_lock":     "必须明确开启或关闭、锁定值；关闭只停止锁定，不恢复历史数值。",
}

var actionWords = map[string]map[string]string{
	"set_player_mode":         {"invincible": "普通无敌 仅免伤", "super_god": "超级无敌 超级上帝", "creative": "统一制造模式 统一创造模式", "health_lock": "最低生命锁定 锁血一滴", "one_hit": "一击必杀 必杀模式 一击秒杀", "invisible": "隐身模式", "water_walk": "踏水模式", "noclip": "穿墙 穿墙模式", "clear_all": "解除全部模式 关闭全部面板模式"},
	"set_daylight_mode":       {"day": "全天白昼 永昼", "night": "全天黑夜 永夜 日食", "restore": "恢复昼夜 解除永昼 解除永夜"},
	"spawn_special_structure": {"training_dummy": "攻速人偶 伤害人偶 DPS人偶", "wormhole_pair": "链接虫洞 成对虫洞", "nightmare_werepig": "梦魇疯猪 束缚疯猪", "celestial_materials": "天体英雄召唤材料 天体祭坛", "scrap_daywalker": "刷新拾荒疯猪 拾荒疯猪"},
	"spawn_entity_array":      {"rock_tree_1": "巨石树阵列一 岩石树阵列一", "rock_tree_2": "巨石树阵列二 岩石树阵列二", "beebox": "蜂箱阵列", "pond": "池塘阵列"},
	"set_wurt_form":           {"normal": "沃特普通形态", "royal": "沃特皇家形态"},
	"apply_all_players":       {"restore": "全员状态恢复", "kill": "全员死亡", "resurrect": "全员复活"},
	"trigger_world_event": {
		"lightning": "闪电 打雷 雷击", "earthquake": "地震 震动", "lunar_hail": "月雹 月亮冰雹", "meteor_shower": "流星雨 陨石 陨石雨",
		"acid_rain": "酸雨", "moon_storm": "月球风暴 月亮风暴", "nightmare_calm": "远古平静 噩梦平静", "nightmare_warn": "远古警告 噩梦警告",
		"nightmare_wild": "远古暴动 噩梦暴动", "nightmare_dawn": "远古消退 噩梦消退", "reset_ruins": "重置远古 刷新远古",
	},
	"set_player_ability": {
		"no_cooldown": "无冷却 攻击无冷却", "one_hit_kill": "一击必杀 必杀 秒杀", "aoe_attack": "范围攻击 群体攻击",
		"critical_work": "高效工作 工作倍率", "water_walk": "踏水 水上行走 水上走路", "knockback_immunity": "免疫击退 防击退",
		"no_hate": "消除仇恨 不被攻击", "stealth": "潜行 隐藏模型", "unlock_recipes": "解锁配方 解锁科技 全部配方",
	},
	"set_player_lock": {"health": "锁血 生命下限 生命锁定", "moisture": "锁定潮湿 防潮 湿度锁定", "temperature": "锁定体温 恒温 温度锁定"},
	"act_nearby_entities": {
		"delete": "删除附近实体 清除附近实体", "extinguish": "附近灭火 扑灭 灭火", "ignite": "点燃附近实体 放火",
		"restore": "恢复附近生物 回满附近生物状态", "repair": "修复耐久 维修地面装备", "repair_burnt": "重建烧毁建筑 修复烧毁建筑", "repair_boat": "修船 修复船只",
		"freshen": "保鲜 恢复新鲜度", "heat": "加热附近物品 加热暖石", "cool": "冷却附近物品 冷却暖石",
		"salvage": "打捞", "haunt": "作祟", "fertilize": "施肥", "grow": "推进生长 生长一阶段", "ripen": "催熟 催生", "harvest": "采摘植物 收获 收割",
		"pick": "拾取地面物品 拾取 掉落物", "chop": "砍树 伐木", "mine": "挖矿 采矿", "hammer": "锤毁 拆建筑", "dig": "挖掘 铲除",
		"till": "耕地 犁地", "build_complete": "完成建造 完成施工", "chaos": "混战 生物互相攻击", "electrocute": "电击",
		"kill": "击杀附近生物 杀死附近生物", "freeze": "冻结附近生物 冻住 冰冻 冻结", "sleep": "催眠 使生物睡觉",
		"panic": "惊吓 恐慌", "pacify": "平息附近仇恨 安抚生物", "root": "定身 禁锢", "taunt": "嘲讽 吸引仇恨",
	},
	"set_character_power": {
		"beard_none": "移除胡须 刮胡子 韦伯剃胡须 威尔逊剃胡须", "beard_short": "短胡须 胡须一级 韦伯短胡须 威尔逊短胡须", "beard_medium": "中胡须 胡须二级 韦伯中胡须 威尔逊中胡须", "beard_long": "长胡须 胡须三级 韦伯长胡须 威尔逊长胡须",
		"woby_empty": "沃比饥饿清空", "woby_half": "沃比饥饿一半", "woby_full": "沃比饥饿回满 喂饱沃比",
		"abigail_bond_1": "阿比盖尔等级一 阿比盖尔一级 阿比盖尔等级", "abigail_bond_2": "阿比盖尔等级二 阿比盖尔二级 阿比盖尔等级",
		"abigail_bond_3": "阿比盖尔等级三 阿比盖尔三级 阿比盖尔等级", "abigail_health_0": "阿比盖尔生命清空 阿比盖尔死亡", "abigail_health_5": "阿比盖尔生命百分之五", "abigail_health_50": "阿比盖尔生命一半", "abigail_health_100": "阿比盖尔生命回满",
		"abigail_shadow": "阿比盖尔暗影", "abigail_lunar_toggle": "阿比盖尔月亮", "inspiration_0": "灵感清空", "inspiration_50": "灵感一半", "inspiration_100": "灵感回满",
		"fitness_0": "健身值清空", "fitness_50": "健身值一半", "fitness_100": "健身值回满",
		"woody_beaver": "伍迪变河狸", "woody_goose": "伍迪变大鹅", "woody_moose": "伍迪变鹿", "wx_charge_add": "WX增加电能 机器人充电", "wx_charge_remove": "WX减少电能",
		"wormwood_bloom_0": "沃姆伍德开花零级", "wormwood_bloom_1": "沃姆伍德开花一级", "wormwood_bloom_2": "沃姆伍德开花二级", "wormwood_bloom_3": "沃姆伍德开花三级",
		"wormwood_bloom_progress_start": "开花进度开始", "wormwood_bloom_progress_half": "开花进度一半", "wormwood_bloom_progress_end": "开花进度完成",
		"wormwood_bloom_grow": "开始开花", "wormwood_bloom_decay": "开始凋谢", "merm_king_hunger_0": "鱼人王饥饿清空", "merm_king_hunger_50": "鱼人王饥饿一半", "merm_king_hunger_100": "鱼人王饥饿回满",
		"merm_king_health_5": "鱼人王生命百分之五", "merm_king_health_50": "鱼人王生命一半", "merm_king_health_100": "鱼人王生命回满",
		"merm_king_trident": "鱼人王三叉戟", "merm_king_crown": "鱼人王王冠", "merm_king_shoulder": "鱼人王肩甲",
	},
}
