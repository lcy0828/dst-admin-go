package console

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"

	"github.com/google/uuid"
)

// The directory describes code-owned operations. Neither editable definitions nor
// client metadata can add an operation, parameter, or executable expression.
func expansionDefinitions() []Definition {
	player := Parameter{Name: "player_id", Label: "玩家 KU ID", Type: "string", Required: true}
	text := func(name, label string) Parameter {
		return Parameter{Name: name, Label: label, Type: "string", Required: true}
	}
	choice := func(name, label string, values ...string) Parameter {
		return Parameter{Name: name, Label: label, Type: "enum", Required: true, Options: values}
	}
	number := func(name, label string, low, high, def int) Parameter {
		return Parameter{Name: name, Label: label, Type: "number", Required: true, Minimum: &low, Maximum: &high, Default: def}
	}
	integer := func(name, label string, low, high, def int) Parameter {
		p := number(name, label, low, high, def)
		p.Type = "integer"
		return p
	}
	boolean := func(name, label string) Parameter {
		return Parameter{Name: name, Label: label, Type: "boolean", Required: true, Default: false}
	}
	def := func(id, name, description string, risk Risk, params ...Parameter) Definition {
		return Definition{ID: id, Name: name, Description: description, Category: "游戏工作台", Risk: risk, IsBuiltin: true, Parameters: params}
	}
	return []Definition{
		def("set_daylight_mode", "设置全天白昼或黑夜", "参考面板的日食按钮实际为全天白昼/黑夜；恢复只撤销本次持有的昼夜分段。", RiskHigh, choice("mode", "昼夜方式", "day", "night", "restore")),
		def("set_nearby_movement", "允许或禁止附近生物移动", "设置面板独立的生物移动倍率，禁止为0、恢复移除此来源；不控制玩家。", RiskHigh, player, integer("radius", "范围", 1, 64, 10), boolean("enabled", "允许移动")),
		def("fertilize_farm_soil", "补充农田养分和水分", "预览附近农田地块，补满三类养分和土壤水分；每批最多25格。", RiskHigh, player, integer("radius", "范围", 1, 20, 8)),
		def("give_character_supplies", "发放角色用品", "只发放服务端维护的角色用品清单，数量1至40；所选玩家必须是相应角色。", RiskHigh, player, choice("character", "角色", "wilson", "willow", "wolfgang", "wendy", "wx78", "wickerbottom", "woodie", "wes", "waxwell", "wathgrithr", "webber", "winona", "warly", "wortox", "wormwood", "wurt", "walter", "wanda"), text("prefab", "角色用品代码"), integer("count", "数量", 1, 40, 1)),
		def("give_turf", "给予地皮物品", "从原生地皮定义获取对应物品，单次给予20个，不能填写任意函数或地皮编号。", RiskHigh, player, choice("tile", "地皮", "ROAD", "PEBBLEBEACH", "MONKEY_GROUND", "SHELLBEACH", "MARSH", "ROCKY", "SAVANNA", "FOREST", "GRASS", "DIRT", "DECIDUOUS", "DESERT_DIRT", "CAVE", "FUNGUS", "FUNGUSRED", "FUNGUSGREEN", "FUNGUSMOON", "SINKHOLE", "UNDERROCK", "VENT", "MUD", "ARCHIVE", "VAULT_CLEAN", "BRICK_GLOW", "BRICK", "TILES_GLOW", "TILES", "TRIM_GLOW", "TRIM", "METEOR", "SCALE", "WOODFLOOR", "CHECKER", "MOSAIC_GREY", "MOSAIC_RED", "MOSAIC_BLUE", "CARPET2", "CARPET", "BEARD_RUG", "LAVAARENA_TRIM")),
		def("edit_world_tiles", "铺设地皮", "先预览所选地块，再确认铺设；每批最多25格，只替换可铺设的陆地。", RiskCritical, choice("tile", "地皮", "QUAGMIRE_GATEWAY", "QUAGMIRE_CITYSTONE", "QUAGMIRE_PARKFIELD", "QUAGMIRE_PARKSTONE", "QUAGMIRE_PEATFOREST", "ROAD", "COTL_BRICK", "PEBBLEBEACH", "MONKEY_GROUND", "SHELLBEACH", "MARSH", "LUNAR_MARSH", "SHADOW_MARSH", "ROCKY", "SAVANNA", "FOREST", "GRASS", "DIRT", "DECIDUOUS", "DESERT_DIRT", "CAVE", "FUNGUS", "FUNGUSRED", "FUNGUSGREEN", "FUNGUSMOON", "SINKHOLE", "UNDERROCK", "VENT", "MUD", "ARCHIVE", "VAULT", "VAULT_CLEAN", "BRICK_GLOW", "BRICK", "TILES_GLOW", "TILES", "TRIM_GLOW", "TRIM", "METEOR", "MONKEY_DOCK", "OCEAN_ICE", "CHARLIE_VINE", "ROPE_BRIDGE", "WAGSTAFF_FLOOR", "SCALE", "WOODFLOOR", "COTL_GOLD", "CHECKER", "MOSAIC_GREY", "MOSAIC_RED", "MOSAIC_BLUE", "CARPET2", "CARPET", "QUAGMIRE_SOIL", "BEARD_RUG", "FARMING_SOIL", "LAVAARENA_TRIM", "LAVAARENA_FLOOR", "RIFT_MOON"), number("x", "中心 X", -16000, 16000, 0), number("z", "中心 Z", -16000, 16000, 0), integer("size", "边长（格）", 1, 5, 1)),
		def("spawn_entity_array", "生成整齐排列的实体", "在安全地面或水域生成阵列，最多25个；巨石树支持两种排列，生成前核对落点。", RiskCritical, player, choice("layout", "排列", "rock_tree_1", "rock_tree_2", "beebox", "pond"), integer("count", "数量", 1, 25, 4), number("spacing", "间距", 4, 24, 6)),
		def("spawn_entity_at", "在地图选点生成", "在所选坐标生成已注册实体；每批最多20个，不接收脚本。", RiskCritical, player, text("prefab", "物品或生物代码"), number("x", "X", -16000, 16000, 0), number("z", "Z", -16000, 16000, 0), integer("count", "数量", 1, 20, 1), Parameter{Name: "skin", Label: "皮肤代码（可选）", Type: "string"}),
		def("spawn_near_player", "在玩家附近批量生成", "预览玩家附近安全落点后生成已注册实体；支持玩家拥有的皮肤，每批最多40个。", RiskCritical, player, text("prefab", "物品或生物代码"), integer("count", "数量", 1, 40, 1), Parameter{Name: "skin", Label: "皮肤代码（可选）", Type: "string"}),
		def("count_world_entities", "统计全世界同类数量", "统计所选世界，包括堆叠数量；可包含玩家和容器中的物品。达到扫描时限会明确失败，不返回伪造的0。", RiskLow, text("prefab", "物品或生物代码"), boolean("include_inventory", "包含背包和箱子")),
		def("announce_entity_count", "公告全世界同类数量", "先统计所选世界（含背包箱子），确认后向游戏发送数量公告。", RiskHigh, text("prefab", "物品或生物代码")),
		def("remove_world_entities", "移除世界内同类实体", "预览并移除所选世界同类实体，默认保留背包箱子内物品；每批最多40个，超限不执行。", RiskCritical, text("prefab", "物品或生物代码"), boolean("include_inventory", "也删除背包和箱子内物品")),
		def("cycle_entity_teleport", "传送到下一个同类实体", "按实体编号轮流定位，核对安全落点后传送；同一玩家各自记住上一次位置。", RiskHigh, player, text("prefab", "实体代码")),
		def("teleport_world_center", "传送到地图中心", "优先检查地图中心附近的安全陆地，找不到则不传送。", RiskHigh, player),
		def("migrate_player", "传送到另一个世界", "通过游戏原生迁移流程前往同一集群已连接的世界；返回已提交不代表已到达，不自动重复迁移。", RiskHigh, player, text("destination_shard", "目标游戏分片编号"), integer("portal_id", "目标入口编号", 1, 10000, 1)),
		def("unlock_player_recipe", "解锁单个配方", "为在线玩家解锁指定原生配方，沿用游戏配方权限。", RiskHigh, player, text("recipe", "配方代码")),
		def("give_starter_kit", "发放基础工具", "给予斧头、鹤嘴锄和铲子各一件；空间不足时保留地面掉落并回报。", RiskHigh, player),
		def("restore_player_stats", "恢复玩家状态", "恢复生命、饥饿、理智、湿度和体温，不改变角色或其他模组的模式。", RiskHigh, player),
		def("set_player_health_value", "设置玩家生命值", "设置绝对生命值，可设为1；不对幽灵或旺达使用普通生命规则。", RiskHigh, player, number("value", "生命值", 1, 99999, 1)),
		def("match_world_temperature", "体温设为世界温度", "读取当前世界温度并设置玩家体温。", RiskHigh, player),
		def("apply_all_players", "操作本世界全部在线玩家", "预览冻结当前分片玩家名单，再执行全体恢复、死亡或复活；其他世界需分别操作。", RiskCritical, choice("action", "操作", "restore", "kill", "resurrect")),
		def("set_world_day", "跳到指定天数", "只能向前推进；先显示需跳过的天数，单次最多200天。", RiskHigh, integer("day", "目标天数", 1, 100000, 1)),
		def("set_nightmare_segments", "设置远古周期时长", "分别设置四阶段的时间段数量，不重置其他世界计时器。", RiskHigh, integer("calm", "平静时段", 1, 64, 4), integer("warn", "警告时段", 1, 64, 1), integer("wild", "暴动时段", 1, 64, 2), integer("dawn", "消退时段", 1, 64, 1)),
		def("remove_world_roads", "移除道路", "移除道路并在保存完成后重载游戏；房间内玩家会断线，请先备份。未收到完成回执时不要重复提交。", RiskCritical),
		def("announce_game_value", "查询并公告游戏数值", "查询幸运值、淘气值或隐士装饰评分，确认后发送游戏公告。", RiskHigh, player, choice("value", "查询项目", "luck", "naughtiness", "pearl_decoration")),
		def("move_nearby_creatures", "引导附近生物移动", "预览半径内有行走能力的非玩家生物，向指定安全地面移动；不传送，不接管玩家。", RiskHigh, player, integer("radius", "范围", 1, 64, 10), number("x", "目标 X", -16000, 16000, 0), number("z", "目标 Z", -16000, 16000, 0)),
		def("ripen_giant_crops", "催熟巨大农作物", "对原版农作物清除生长压力并有界推进至成熟；不处理未知模组植物。", RiskHigh, player, integer("radius", "范围", 1, 64, 10)),
		def("spawn_special_structure", "生成特殊游戏设施", "使用固定原生预制物与组件：攻速人偶、成对虫洞、梦魇疯猪、天体英雄材料或拾荒疯猪刷新。", RiskCritical, player, choice("kind", "设施", "training_dummy", "wormhole_pair", "nightmare_werepig", "celestial_materials", "scrap_daywalker"), number("x", "第二端 X（虫洞）", -16000, 16000, 0), number("z", "第二端 Z（虫洞）", -16000, 16000, 0)),
		def("set_wurt_form", "切换沃特形态", "仅切换沃特的普通或皇家形态，保留当前三维比例，不创建或删除鱼人王。", RiskHigh, player, choice("form", "形态", "normal", "royal")),
		def("repair_skill_sync", "重新同步技能数据", "重新向玩家同步原生技能数据；客户端弹窗仍异常时需要重新登录，不能在服务端强改客户端界面。", RiskHigh, player),
		def("reveal_player_map", "为玩家显示本世界地图", "永久探索本世界地图；确认后分帧展开，不修改其他玩家地图。", RiskHigh, player),
		def("set_player_mode", "设置玩家模式", "独立控制八种模式；关闭只撤销面板持有的设置，不撤销其他模组的效果。", RiskHigh, player, choice("mode", "模式", "invincible", "super_god", "creative", "health_lock", "one_hit", "invisible", "water_walk", "noclip", "clear_all"), boolean("enabled", "开启")),
		def("set_mode_memory", "设置玩家模式记忆", "默认关闭。开启后仅记住本入口明确设置的模式，重进本世界后恢复；关闭可同时清除记忆。", RiskHigh, player, boolean("enabled", "记住本玩家模式")),
	}
}

var expansionToken = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)
var shardToken = regexp.MustCompile(`^[0-9]{1,10}$`)

func expansionDefinition(id string) (Definition, bool) {
	for _, d := range expansionDefinitions() {
		if d.ID == id {
			return d, true
		}
	}
	return Definition{}, false
}
func guardedExpansion(id string) bool {
	_, ok := expansionDefinition(id)
	return ok && id != "count_world_entities"
}

func addExpansionTemplates(templates map[string]template) {
	for _, definition := range expansionDefinitions() {
		d := definition
		if d.ID == "edit_world_tiles" || d.ID == "spawn_entity_at" {
			d.Parameters = append(d.Parameters, Parameter{Name: "expected_session", Label: "地图存档标识", Type: "string", Description: "从所选地图读取；存档已更换时拒绝执行。"})
		}
		if guardedExpansion(d.ID) {
			d.Parameters = append(d.Parameters, Parameter{Name: "feedback", Label: "额外提示", Type: "enum", Options: []string{"panel_detail", "panel_compact", "panel_chat", "nearby_speech", "world_chat", "off"}, Default: "panel_detail"})
			d.Parameters = append(d.Parameters, Parameter{Name: "phase", Label: "操作步骤", Type: "enum", Options: []string{"preview", "execute", "cancel"}, Default: "preview"}, Parameter{Name: "preview_id", Label: "确认编号", Type: "string"})
		}
		templates[d.ID] = template{definition: d, render: func(args map[string]interface{}) (string, error) {
			clean := map[string]interface{}{}
			allowed := map[string]bool{}
			for _, p := range d.Parameters {
				allowed[p.Name] = true
				v, ok := args[p.Name]
				if !ok {
					if p.Required {
						return "", ErrInvalidArguments
					}
					if p.Default != nil {
						v = p.Default
					} else {
						continue
					}
				}
				switch p.Type {
				case "string", "enum":
					s, ok := v.(string)
					if !ok || len(s) > 128 {
						return "", ErrInvalidArguments
					}
					if p.Type == "enum" {
						found := false
						for _, opt := range p.Options {
							if s == opt {
								found = true
							}
						}
						if !found {
							return "", ErrInvalidArguments
						}
					}
					switch p.Name {
					case "player_id":
						if !dstUserIDPattern.MatchString(s) {
							return "", ErrInvalidArguments
						}
					case "prefab", "recipe", "skin":
						if (s == "" && p.Required) || s != "" && !expansionToken.MatchString(s) {
							return "", ErrInvalidArguments
						}
					case "expected_session":
						if s != "" && !regexp.MustCompile(`^[A-Fa-f0-9]{1,64}$`).MatchString(s) {
							return "", ErrInvalidArguments
						}
					case "destination_shard":
						if !shardToken.MatchString(s) {
							return "", ErrInvalidArguments
						}
					}
				case "integer", "number":
					n, ok := numericArgument(v)
					if !ok || math.IsNaN(n) || math.IsInf(n, 0) || p.Type == "integer" && math.Trunc(n) != n || p.Minimum != nil && n < float64(*p.Minimum) || p.Maximum != nil && n > float64(*p.Maximum) {
						return "", ErrInvalidArguments
					}
				case "boolean":
					if _, ok := v.(bool); !ok {
						return "", ErrInvalidArguments
					}
				}
				if p.Name != "phase" && p.Name != "preview_id" {
					clean[p.Name] = v
				}
			}
			for k := range args {
				if !allowed[k] {
					return "", ErrInvalidArguments
				}
			}
			if d.ID == "set_player_mode" && clean["mode"] == "clear_all" && clean["enabled"] == true {
				return "", ErrInvalidArguments
			}
			phase := nearbyPhase(args)
			token, _ := args["preview_id"].(string)
			if guardedExpansion(d.ID) && phase != "preview" {
				if _, e := uuid.Parse(token); e != nil {
					return "", ErrInvalidArguments
				}
			}
			payload, e := json.Marshal(clean)
			if e != nil {
				return "", ErrInvalidArguments
			}
			// JSON is quoted as one Lua string; keys and values are never executable Lua.
			return fmt.Sprintf(`local r=rawget(_G,"DSTAdmin");local w=r and r.Commands and r.Commands.Workbench;if not w or w.ExpansionVersion~=1 then error("WORKBENCH_UPDATE_REQUIRED")end;w.Expansion(%s,require("json").decode(%s),%s,%s,%s,_dst_admin_batch_caller,_dst_admin_batch_request)`, quoteLua(d.ID), quoteLua(string(payload)), quoteLua(phase), quoteLua(token), quoteLua(uuid.NewString())), nil
		}}
	}
}
