package aicontrol

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dont/internal/apicontract"
)

// Real user phrasings from the 2026-11-05 report must reach the capability the
// panel already has. Discovery never executes.
func TestRegistryDiscoveryResolvesReportedPhrasings(t *testing.T) {
	s, _ := botFixture(t, apiBackendFunc(func(context.Context, Principal, *apicontract.Operation, apicontract.Input) (json.RawMessage, error) {
		t.Fatal("discovery must not execute an API")
		return nil, nil
	}))
	manager := taskPrincipal(ProfileGameManage)
	for _, tc := range []struct{ query, capability string }{
		{"跳过一天", "workbench.skip_days"},
		{"帮我跳过两天", "workbench.skip_days"},
		{"快进3天", "workbench.skip_days"},
		{"直接跳到明天", "workbench.skip_days"},
		{"重置世界", "room.reset"},
		{"重新生成世界", "room.reset"},
		{"删档重开", "room.reset"},
		{"最近一天服务器CPU内存", "room.performance"},
		{"最近一天 CPU 内存怎么样", "room.performance"},
		{"今天服务器卡不卡", "room.performance"},
		{"给玩家lcy 发五个草", "player.give_item"},
		{"给玩家lcy发五个草", "player.give_item"},
		{"给lcy发5个草", "player.give_item"},
		{"开启无敌模式", "player.god_mode"},
		{"给lcy开启无敌模式", "player.god_mode"},
		{"开启制作模式", "player.creative_mode"},
		{"关掉lcy的创造模式", "player.creative_mode"},
		{"复活lcy", "player.resurrect"},
		{"重置玩家lcy", "player.reset_character"},
		{"lcy开了无敌吗", "world.player_modes"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			r := callTask(t, s, manager, "dst_find", map[string]any{"query": tc.query})
			items := r.Data.(map[string]any)["items"].([]map[string]any)
			if len(items) == 0 || items[0]["id"] != tc.capability || r.TaskRef != "" {
				t.Fatalf("got %#v", items)
			}
			if items[0]["tier"] == "" || items[0]["confirm"] == "" {
				t.Fatal("policy missing from discovery", items[0])
			}
		})
	}
	// Negatives: generic words must not leak into unrelated capabilities.
	for _, tc := range []struct{ query, forbidden string }{
		{"跳过这个话题", "workbench.skip_days"},
		{"查看地图", "player.resolve"},
		{"查看地图", "workbench.reveal_player_map"},
		{"重置世界", "player.reset_character"},
		{"开启无敌模式", "workbench.set_player_invincibility"},
		{"开启无敌模式", "workbench.set_player_mode"},
	} {
		r := callTask(t, s, manager, "dst_find", map[string]any{"query": tc.query})
		for _, item := range r.Data.(map[string]any)["items"].([]map[string]any) {
			if item["id"] == tc.forbidden {
				t.Fatalf("%q leaked to %s", tc.query, tc.forbidden)
			}
		}
	}
}

func TestPreferredCanonicalOutranksDuplicateImplementation(t *testing.T) {
	s, _ := taskFixture(t, nil)
	manager := taskPrincipal(ProfileGameManage)
	// "普通无敌" names both duplicates exactly; neither may outrank god_mode
	// when god_mode also matches.
	r := callTask(t, s, manager, "dst_find", map[string]any{"query": "无敌"})
	items := r.Data.(map[string]any)["items"].([]map[string]any)
	if len(items) == 0 || items[0]["id"] != "player.god_mode" {
		t.Fatal(items)
	}
}

func TestRegistryMetadataDoesNotChangeSchemaRevision(t *testing.T) {
	for _, c := range BotCapabilities() {
		stripped := c
		stripped.Aliases, stripped.Examples, stripped.Negatives = nil, nil, nil
		stripped.Tier, stripped.Confirm, stripped.Prefer = "", "", ""
		stripped.SlotRoles, stripped.Receipt, stripped.ActionPolicies, stripped.SearchTerms = nil, nil, nil, nil
		if botSchemaRevision(stripped) != c.SchemaRevision {
			t.Fatalf("%s: registry language changed the frozen schema revision", c.ID)
		}
	}
}

func TestPerformanceWindowMapsToBoundedReportQuery(t *testing.T) {
	var c BotCapability
	for _, v := range BotCapabilities() {
		if v.ID == "room.performance" {
			c = v
		}
	}
	if c.SlotRoles["window"].Role != "time_range" {
		t.Fatal(c.SlotRoles)
	}
	in := c.build("room-a", map[string]any{})
	if in.Query["maxPoints"] != 60 || in.Query["from"] != nil {
		t.Fatal("default must keep the backend 24 hour range", in.Query)
	}
	in = c.build("room-a", map[string]any{"window": "7d", "worldId": "w1"})
	from, err := time.Parse(time.RFC3339, in.Query["from"].(string))
	if err != nil || time.Since(from) < 7*24*time.Hour-time.Minute || time.Since(from) > 7*24*time.Hour+time.Minute || in.Query["worldId"] != "w1" || in.Query["maxPoints"] != 60 {
		t.Fatal(in.Query)
	}
	in = c.build("room-a", map[string]any{"window": "1h", "to": "2026-11-05T12:00:00Z"})
	if in.Query["from"] != "2026-11-05T11:00:00Z" || in.Query["to"] != "2026-11-05T12:00:00Z" {
		t.Fatal(in.Query)
	}
	in = c.build("room-a", map[string]any{"window": "1h", "from": "2026-11-05T00:00:00Z"})
	if in.Query["from"] != "2026-11-05T00:00:00Z" {
		t.Fatal("explicit range must win", in.Query)
	}
}
