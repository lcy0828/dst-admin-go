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
}

// WorkbenchBehaviorRevision invalidates unsubmitted tasks when fixed command
// semantics change without changing their public parameter schema.
const WorkbenchBehaviorRevision = "3"

func commandActions(d Definition) []Action {
	field := map[string]string{"trigger_world_event": "event", "set_player_ability": "ability", "set_character_power": "action", "act_nearby_entities": "action", "set_player_lock": "resource"}[d.ID]
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
			out = append(out, Action{ID: d.ID + "." + value, Title: terms[0], SearchTerms: terms, Parameters: map[string]string{field: value}, Risk: d.Risk, Description: actionDescriptions[d.ID]})
		}
	}
	return out
}

var actionDescriptions = map[string]string{
	"act_nearby_entities": "仅作用于所选世界和范围；需要确认。调用成功不等于所有对象效果均已验证。",
	"set_player_ability":  "必须明确开启或关闭；取决于当前角色和组件。关闭不会撤销已经发生的效果。",
	"set_character_power": "需要对应角色或随从存在于所选世界；不支持时不会替换为其他动作。",
	"trigger_world_event": "改变所选世界；闪电和流星雨还需指定定位玩家。",
	"set_player_lock":     "必须明确开启或关闭、锁定值；关闭只停止锁定，不恢复历史数值。",
}

var actionWords = map[string]map[string]string{
	"trigger_world_event": {
		"lightning": "闪电 打雷 雷击", "earthquake": "地震 震动", "lunar_hail": "月雹 月亮冰雹", "meteor_shower": "流星雨 陨石 陨石雨",
		"acid_rain": "酸雨", "moon_storm": "月球风暴 月亮风暴", "nightmare_calm": "远古平静 噩梦平静", "nightmare_warn": "远古警告 噩梦警告",
		"nightmare_wild": "远古暴动 噩梦暴动", "nightmare_dawn": "远古消退 噩梦消退", "reset_ruins": "重置远古 刷新远古",
	},
	"set_player_ability": {
		"no_cooldown": "无冷却 攻击无冷却", "one_hit_kill": "一击必杀 必杀 秒杀", "aoe_attack": "范围攻击 群体攻击",
		"critical_work": "高效工作 工作倍率", "water_walk": "踏水 水上行走 水上走路", "knockback_immunity": "免疫击退 防击退",
		"no_hate": "消除仇恨 不被攻击", "stealth": "隐身 潜行", "unlock_recipes": "解锁配方 解锁科技 全部配方",
	},
	"set_player_lock": {"health": "锁血 生命下限 生命锁定", "moisture": "锁定潮湿 防潮 湿度锁定", "temperature": "锁定体温 恒温 温度锁定"},
	"act_nearby_entities": {
		"delete": "删除附近实体 清除附近实体", "extinguish": "附近灭火 扑灭 灭火", "ignite": "点燃附近实体 放火",
		"restore": "恢复附近生物 回满附近生物状态", "repair": "修复烧毁建筑 修复建筑", "repair_boat": "修船 修复船只",
		"freshen": "保鲜 恢复新鲜度", "heat": "加热附近物品 加热暖石", "cool": "冷却附近物品 冷却暖石",
		"salvage": "打捞", "haunt": "作祟", "fertilize": "施肥", "grow": "催熟 催生 生长", "harvest": "收获 收割",
		"pick": "采摘 拾取", "chop": "砍树 伐木", "mine": "挖矿 采矿", "hammer": "锤毁 拆建筑", "dig": "挖掘 铲除",
		"till": "耕地 犁地", "build_complete": "完成建造 完成施工", "chaos": "混战 生物互相攻击", "electrocute": "电击",
		"kill": "击杀附近生物 杀死附近生物", "freeze": "冻结附近生物 冻住 冰冻 冻结", "sleep": "催眠 使生物睡觉",
		"panic": "惊吓 恐慌", "pacify": "平息附近仇恨 安抚生物", "root": "定身 禁锢", "taunt": "嘲讽 吸引仇恨",
	},
	"set_character_power": {
		"beard_none": "移除胡须 刮胡子", "beard_short": "短胡须 胡须一级", "beard_medium": "中胡须 胡须二级", "beard_long": "长胡须 胡须三级",
		"woby_empty": "沃比饥饿清空", "woby_half": "沃比饥饿一半", "woby_full": "沃比饥饿回满 喂饱沃比",
		"abigail_bond_1": "阿比盖尔等级一 阿比盖尔一级 阿比盖尔等级", "abigail_bond_2": "阿比盖尔等级二 阿比盖尔二级 阿比盖尔等级",
		"abigail_bond_3": "阿比盖尔等级三 阿比盖尔三级 阿比盖尔等级", "abigail_health_5": "阿比盖尔生命百分之五", "abigail_health_50": "阿比盖尔生命一半", "abigail_health_100": "阿比盖尔生命回满",
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
