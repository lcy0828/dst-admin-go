package intent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dont/internal/capreg"
)

type snapshotCap struct {
	ID             string                   `json:"id"`
	Title          string                   `json:"title"`
	Description    string                   `json:"description"`
	Category       string                   `json:"category"`
	Access         string                   `json:"access"`
	Risk           string                   `json:"risk"`
	Tier           string                   `json:"tier"`
	Confirm        string                   `json:"confirm"`
	SearchTerms    []string                 `json:"searchTerms"`
	Aliases        []string                 `json:"aliases"`
	Examples       []string                 `json:"examples"`
	Negatives      []string                 `json:"negatives"`
	Prefer         string                   `json:"prefer"`
	Parameters     map[string]any           `json:"parameters"`
	ParameterHints map[string]string        `json:"parameterHints"`
	SlotRoles      map[string]capreg.Slot   `json:"slotRoles"`
	ActionPolicies map[string]capreg.Action `json:"actionPolicies"`
	Actions        []struct {
		ID          string         `json:"id"`
		Title       string         `json:"title"`
		Description string         `json:"description"`
		SearchTerms []string       `json:"searchTerms"`
		Parameters  map[string]string `json:"parameters"`
	} `json:"actions"`
}

func loadSnapshotDocs(t *testing.T) []CapabilityDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "capreg", "testdata", "bot_catalog_snapshot.json"))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var items []snapshotCap
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	docs := make([]CapabilityDoc, len(items))
	for i, it := range items {
		actions := make([]ActionDoc, len(it.Actions))
		for j, a := range it.Actions {
			actions[j] = ActionDoc{
				ID:          a.ID,
				Title:       a.Title,
				Description: a.Description,
				SearchTerms: a.SearchTerms,
				Parameters:  a.Parameters,
			}
		}
		docs[i] = CapabilityDoc{
			ID:             it.ID,
			Title:          it.Title,
			Description:    it.Description,
			Category:       it.Category,
			Access:         it.Access,
			Risk:           it.Risk,
			Tier:           it.Tier,
			Confirm:        it.Confirm,
			SearchTerms:    it.SearchTerms,
			Aliases:        it.Aliases,
			Examples:       it.Examples,
			Negatives:      it.Negatives,
			Prefer:         it.Prefer,
			Parameters:     it.Parameters,
			ParameterHints: it.ParameterHints,
			SlotRoles:      it.SlotRoles,
			Actions:        actions,
			ActionPolicies: it.ActionPolicies,
		}
	}
	return docs
}

func TestTagEntitiesStripsPlayerPrefixAndExtractsSlots(t *testing.T) {
	for _, q := range []string{"@bot 给玩家lcy 发五个草", "给玩家lcy发五个草", "请帮我给 lcy 发放 5 个草吧"} {
		tagged := TagEntities(q, Context{})
		if tagged.Player != "lcy" || tagged.Item != "草" || tagged.Count != 5 {
			t.Fatalf("query %q: got player=%q item=%q count=%d", q, tagged.Player, tagged.Item, tagged.Count)
		}
	}

	// Preserve literal "玩家甲" when present in KnownPlayers.
	taggedKnown := TagEntities("给玩家甲发五个草", Context{KnownPlayers: []string{"玩家甲"}})
	if taggedKnown.Player != "玩家甲" {
		t.Fatalf("expected known player 玩家甲 to be preserved, got %q", taggedKnown.Player)
	}

	mode := TagEntities("给玩家lcy开启无敌模式", Context{})
	if mode.Player != "lcy" || mode.Enabled == nil || !*mode.Enabled {
		t.Fatalf("mode tagging failed: %+v", mode)
	}

	skip := TagEntities("跳过一天", Context{})
	if skip.Days != 1 {
		t.Fatalf("expected days=1, got %d", skip.Days)
	}

	perf := TagEntities("查询最近一天服务器的运行状态 cpu 内存指标怎么样", Context{})
	if perf.Window != "24h" || !perf.HasPerfSignal {
		t.Fatalf("expected window=24h and perf signal, got %+v", perf)
	}
}

func TestServiceRankUserReportedQueries(t *testing.T) {
	docs := loadSnapshotDocs(t)
	svc := New(nil)

	cases := []struct {
		query  string
		wantID string
		slots  map[string]any
	}{
		{
			query:  "给玩家lcy 发五个草",
			wantID: "player.give_item",
			slots:  map[string]any{"playerId": "lcy", "item": "草", "count": 5},
		},
		{
			query:  "给玩家lcy发五个草",
			wantID: "player.give_item",
			slots:  map[string]any{"playerId": "lcy", "item": "草", "count": 5},
		},
		{
			query:  "给玩家lcy开启无敌模式",
			wantID: "player.god_mode",
			slots:  map[string]any{"playerId": "lcy", "enabled": true},
		},
		{
			query:  "跳过一天",
			wantID: "workbench.skip_days",
			slots:  map[string]any{"days": 1},
		},
		{
			query:  "重置世界",
			wantID: "room.reset",
		},
		{
			query:  "重置世界 不备份",
			wantID: "room.reset",
			slots:  map[string]any{"backup": false},
		},
		{
			query:  "查询最近一天服务器的运行状态 cpu 内存指标怎么样",
			wantID: "room.performance",
			slots:  map[string]any{"window": "24h"},
		},
		{
			query:  "查看地图",
			wantID: "room.map",
		},
		{
			query:  "复活玩家lcy",
			wantID: "player.resurrect",
			slots:  map[string]any{"player_id": "lcy"},
		},
	}

	for _, tc := range cases {
		res := svc.Rank(context.Background(), tc.query, Context{}, docs, "v1")
		if len(res.Candidates) == 0 {
			t.Fatalf("query %q returned no candidates", tc.query)
		}
		top := res.Candidates[0]
		if top.Capability.ID != tc.wantID {
			t.Fatalf("query %q: got top=%s (score=%d), want %s", tc.query, top.Capability.ID, top.Score, tc.wantID)
		}
		for k, wantV := range tc.slots {
			if gotV := top.ExtractedSlots[k]; gotV != wantV {
				t.Fatalf("query %q slot %s: got %#v, want %#v (all=%#v)", tc.query, k, gotV, wantV, top.ExtractedSlots)
			}
		}
	}
}
