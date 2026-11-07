package aicontrol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskActionDiscoveryProvidesEnumArgumentsWithoutExecuting(t *testing.T) {
	s, _ := taskFixture(t, nil)
	manager := taskPrincipal(ProfileGameManage)
	for _, tc := range []struct{ query, command, field, value string }{
		{"穿墙", "set_player_mode", "mode", "noclip"},
		{"超级无敌", "set_player_mode", "mode", "super_god"},
		{"蜂箱阵列", "spawn_entity_array", "layout", "beebox"},
		{"攻速人偶", "spawn_special_structure", "kind", "training_dummy"},
		{"沃特皇家形态", "set_wurt_form", "form", "royal"},
		{"全天黑夜", "set_daylight_mode", "mode", "night"},
		{"闪电", "trigger_world_event", "event", "lightning"},
		{"月雹", "trigger_world_event", "event", "lunar_hail"},
		{"隐身", "set_player_mode", "mode", "invisible"},
		{"潜行", "set_player_ability", "ability", "stealth"},
		{"踏水", "set_player_mode", "mode", "water_walk"},
		{"一击必杀", "set_player_mode", "mode", "one_hit"},
		{"冻结附近生物", "act_nearby_entities", "action", "freeze"},
		{"附近灭火", "act_nearby_entities", "action", "extinguish"},
		{"阿比盖尔三级", "set_character_power", "action", "abigail_bond_3"},
		{"韦伯长胡须", "set_character_power", "action", "beard_long"},
		{"别冻结蜘蛛", "act_nearby_entities", "action", "freeze"},
		{"冻结会不会影响玩家", "act_nearby_entities", "action", "freeze"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := callTask(t, s, manager, "dst_find", map[string]any{"query": tc.query})
			items := r.Data.(map[string]any)["items"].([]map[string]any)
			if len(items) == 0 || items[0]["id"] != "workbench."+tc.command || r.TaskRef != "" {
				t.Fatal(r)
			}
			input := items[0]["input"].(map[string]any)
			if input["parameters"].(map[string]string)[tc.field] != tc.value {
				t.Fatal(input)
			}
			if _, ok := input["parameters"].(map[string]string)["confirmed"]; ok {
				t.Fatal("fabricated confirmation")
			}
			r = callTask(t, s, taskPrincipal(ProfileGameRead), "dst_find", map[string]any{"query": tc.query})
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "workbench.") {
				t.Fatalf("read-only caller saw write actions: %s", b)
			}
		})
	}
}

func TestActionDescriptionsDoNotChangeFrozenExecutionButBehaviorDoes(t *testing.T) {
	for _, c := range BotCapabilities() {
		if len(c.Actions) == 0 {
			continue
		}
		old := botSchemaRevision(c)
		c.Title = "更易懂的说明"
		c.Actions[0].Title = "新名称"
		if botSchemaRevision(c) != old {
			t.Fatal("description invalidates task")
		}
		c.BehaviorRevision += ".changed"
		if botSchemaRevision(c) == old {
			t.Fatal("behavior change permits stale execution")
		}
	}
}

func TestPlayerModeDiscoveryKeepsImmunityAndGodEffectsSeparate(t *testing.T) {
	s, _ := taskFixture(t, nil)
	for query, want := range map[string]string{
		"普通无敌": "workbench.set_player_invincibility",
		"仅免伤":  "workbench.set_player_invincibility",
		"无敌模式": "player.god_mode",
		"上帝模式": "player.god_mode",
	} {
		t.Run(query, func(t *testing.T) {
			r := callTask(t, s, taskPrincipal(ProfileGameManage), "dst_find", map[string]any{"query": query})
			items := r.Data.(map[string]any)["items"].([]map[string]any)
			if len(items) == 0 || items[0]["id"] != want {
				t.Fatalf("mode discovery changed the effect: %+v", r)
			}
		})
	}
}
