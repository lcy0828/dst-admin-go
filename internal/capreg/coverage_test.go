package capreg_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"dont/internal/aicontrol"
	"dont/internal/capreg"
	consoleapi "dont/internal/console"
)

var update = flag.Bool("update", false, "rewrite testdata/bot_catalog_snapshot.json")

// Prefer targets that are planned but not yet registered as bot capabilities.
// room.reset (backup then regenerate) is added by a follow-up work package.
var pendingPreferTargets = map[string]bool{}

// Generic words that collide across capabilities must carry these negatives.
var requiredNegatives = map[string][]string{
	"workbench.skip_days":                {"跳过这个话题"},
	"player.resolve":                     {"查看地图"},
	"player.trajectory":                  {"查看地图"},
	"workbench.reveal_player_map":        {"查看地图"},
	"workbench.regenerate":               {"重置玩家"},
	"player.reset_character":             {"重置世界"},
	"workbench.set_player_invincibility": {"开启无敌模式"},
	"world.player_modes":                 {"开启无敌模式"},
}

const (
	minAliases        = 3
	minExamples       = 6
	minActionAliases  = 2
	minActionExamples = 2
)

func capabilities(t *testing.T) map[string]aicontrol.BotCapability {
	t.Helper()
	out := map[string]aicontrol.BotCapability{}
	for _, c := range aicontrol.BotCapabilities() {
		out[c.ID] = c
	}
	return out
}

func TestRegistryCoversEveryCapabilityWithoutOrphans(t *testing.T) {
	caps := capabilities(t)
	// Language enhancements are optional; native definitions are sufficient.
	for _, e := range capreg.All() {
		if _, ok := caps[e.ID]; !ok {
			t.Errorf("orphan registry entry %s", e.ID)
		}
	}
}

func TestRegistryCoversEveryWorkbenchDefinitionAndAction(t *testing.T) {
	for _, d := range consoleapi.BuiltinWorkbenchDefinitions() {
		id := "workbench." + d.ID
		if d.ID == "resurrect_player" {
			id = "player.resurrect"
		}
		e, ok := capreg.Lookup(id)
		if !ok {
			continue
		}
		keys := map[string]bool{}
		for _, a := range d.Actions {
			key := strings.TrimPrefix(a.ID, d.ID+".")
			keys[key] = true
		}
		for key := range e.Actions {
			if !keys[key] {
				t.Errorf("%s: orphan action %s", id, key)
			}
		}
	}
}

func TestRegistryQualityThresholds(t *testing.T) {
	caps := capabilities(t)
	for _, e := range capreg.All() {
		if len(e.Aliases) < minAliases || len(e.Examples) < minExamples {
			t.Errorf("%s: %d aliases / %d examples, want >= %d / %d", e.ID, len(e.Aliases), len(e.Examples), minAliases, minExamples)
		}
		if len(e.Negatives) == 0 {
			t.Errorf("%s: no negatives", e.ID)
		}
		for _, want := range requiredNegatives[e.ID] {
			found := false
			for _, n := range e.Negatives {
				found = found || n == want
			}
			if !found {
				t.Errorf("%s: missing negative %q", e.ID, want)
			}
		}
		for key, a := range e.Actions {
			if len(a.Aliases) < minActionAliases || len(a.Examples) < minActionExamples {
				t.Errorf("%s.%s: %d aliases / %d examples", e.ID, key, len(a.Aliases), len(a.Examples))
			}
		}
		c := caps[e.ID]
		// Reads are always R0; a write may be R0 only when it is a read
		// implemented through the workbench and prefers a real read capability.
		if c.Access == "read" && e.Tier != capreg.TierRead {
			t.Errorf("%s: read capability with tier %s", e.ID, e.Tier)
		}
		if c.Access == "write" && e.Tier == capreg.TierRead {
			if p, ok := caps[e.Prefer]; !ok || p.Access != "read" {
				t.Errorf("%s: R0 write must prefer a read capability", e.ID)
			}
		}
		// A parent never claims a weaker tier than any of its actions.
		for key, a := range e.Actions {
			if a.Tier > e.Tier {
				t.Errorf("%s.%s: action tier %s above parent %s", e.ID, key, a.Tier, e.Tier)
			}
		}
	}
}

func TestNegativesNeverSuppressOwnLanguage(t *testing.T) {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), "")) }
	check := func(where string, negatives, positives []string) {
		for _, n := range negatives {
			for _, p := range positives {
				if strings.Contains(norm(p), norm(n)) {
					t.Errorf("%s: negative %q suppresses own phrase %q", where, n, p)
				}
			}
		}
	}
	for _, e := range capreg.All() {
		check(e.ID, e.Negatives, append(append([]string{}, e.Aliases...), e.Examples...))
		for key, a := range e.Actions {
			check(e.ID+"."+key, append(append([]string{}, e.Negatives...), a.Negatives...), append(append([]string{}, a.Aliases...), a.Examples...))
		}
	}
}

func TestEveryParameterHasSlotRole(t *testing.T) {
	for _, c := range aicontrol.BotCapabilities() {
		e, _ := capreg.Lookup(c.ID)
		props, _ := c.Parameters["properties"].(map[string]any)
		for name, raw := range props {
			slot, ok := e.Slot(name)
			if !ok {
				t.Errorf("%s: parameter %s has no slot role", c.ID, name)
				continue
			}
			if c.SlotRoles[name] != slot {
				t.Errorf("%s: slotRoles[%s] not published", c.ID, name)
			}
			schema, _ := raw.(map[string]any)
			if slot.Role == "presentation" {
				if _, ok := schema["default"]; !ok {
					t.Errorf("%s: presentation parameter %s has no default", c.ID, name)
				}
			}
		}
		for name := range e.Slots() {
			if _, ok := props[name]; !ok {
				t.Errorf("%s: slot %s is not a parameter", c.ID, name)
			}
		}
	}
}

type owner struct{ capability, action, prefer string }

func (o owner) String() string {
	if o.action != "" {
		return o.capability + "." + o.action
	}
	return o.capability
}

// Exact duplicates across different capabilities are allowed only when one
// side declares the other as its preferred canonical capability, or both
// declare the same canonical capability.
func TestNoAmbiguousLanguageAcrossCapabilities(t *testing.T) {
	caps := capabilities(t)
	for _, field := range []string{"alias", "example"} {
		seen := map[string][]owner{}
		for _, e := range capreg.All() {
			list := e.Aliases
			if field == "example" {
				list = e.Examples
			}
			for _, v := range list {
				seen[strings.ToLower(v)] = append(seen[strings.ToLower(v)], owner{e.ID, "", e.Prefer})
			}
			for key, a := range e.Actions {
				prefer := a.Prefer
				if prefer == "" {
					prefer = e.Prefer
				}
				list := a.Aliases
				if field == "example" {
					list = a.Examples
				}
				for _, v := range list {
					seen[strings.ToLower(v)] = append(seen[strings.ToLower(v)], owner{e.ID, key, prefer})
				}
			}
		}
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, v := range keys {
			owners := seen[v]
			for i := range owners {
				for j := i + 1; j < len(owners); j++ {
					a, b := owners[i], owners[j]
					// Same capability, one side canonical for the other, or both
					// duplicates of the same canonical capability.
					if a.capability == b.capability || a.prefer == b.capability || b.prefer == a.capability || a.prefer != "" && a.prefer == b.prefer {
						continue
					}
					t.Errorf("%s %q shared by %s and %s without prefer", field, v, a, b)
				}
			}
		}
	}
	for _, e := range capreg.All() {
		targets := []string{e.Prefer}
		for _, a := range e.Actions {
			targets = append(targets, a.Prefer)
		}
		for _, p := range targets {
			if _, ok := caps[p]; p != "" && !ok && !pendingPreferTargets[p] {
				t.Errorf("%s: prefer target %s does not exist", e.ID, p)
			}
		}
	}
}

func TestRegistryMetadataIsPublished(t *testing.T) {
	for _, c := range aicontrol.BotCapabilities() {
		e, _ := capreg.Lookup(c.ID)
		if c.Tier != string(e.Tier) || c.Confirm != string(e.Confirm) || c.Prefer != e.Prefer || len(c.Aliases) != len(e.Aliases) || len(c.Examples) != len(e.Examples) {
			t.Errorf("%s: registry metadata not published", c.ID)
		}
		if len(c.SearchTerms) == 0 || len(c.SearchTerms) > 32 {
			t.Errorf("%s: %d search terms", c.ID, len(c.SearchTerms))
		}
		for _, a := range c.Actions {
			policy, ok := c.ActionPolicies[a.ID]
			if !ok || fmt.Sprint(a.SearchTerms) != fmt.Sprint(policy.Aliases) {
				t.Errorf("%s: action %s does not use registry language", c.ID, a.ID)
			}
		}
	}
}

// The snapshot is the exact catalog the plugin receives. Plugin tests load it
// so they never test against a synthetic catalog again.
func TestBotCatalogSnapshot(t *testing.T) {
	got, err := json.MarshalIndent(aicontrol.BotCapabilities(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "bot_catalog_snapshot.json")
	if *update || os.Getenv("UPDATE_SNAPSHOT") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/capreg -run Snapshot -update)", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("bot catalog changed; review and run: go test ./internal/capreg -run Snapshot -update")
	}
}
