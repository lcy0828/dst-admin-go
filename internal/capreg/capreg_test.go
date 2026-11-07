package capreg

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinCatalogLoads(t *testing.T) {
	c, err := Parse(catalogYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.All()) == 0 {
		t.Fatal("empty catalog")
	}
	e, ok := Lookup("workbench.skip_days")
	if !ok || e.Tier != TierRoomWide || e.Confirm != ConfirmOnce {
		t.Fatal(e)
	}
	if s, ok := e.Slot("days"); !ok || s.Role != "quantity" || !s.Model {
		t.Fatal(s)
	}
	if s, ok := e.Slot("worldId"); !ok || s.Role != "world" || s.Ask != "never" {
		t.Fatal("default slot not resolved", s)
	}
	if s, ok := e.Slot("feedback"); !ok || s.Model || s.Ask != "never" || s.Role != "presentation" {
		t.Fatal("presentation slot leaked to model", s)
	}
	mode, _ := Lookup("workbench.set_player_mode")
	if a := mode.Actions["invincible"]; a.Prefer != "workbench.set_player_invincibility" || a.Tier != mode.Tier || a.Confirm != mode.Confirm {
		t.Fatal("action did not inherit policy", a)
	}
	// Entry-level slots override defaults with the same name.
	moon, _ := Lookup("workbench.set_moon_phase")
	if s, _ := moon.Slot("phase"); s.Role != "enum" || !s.Model {
		t.Fatal("entry slot did not override default", s)
	}
}

func TestParseRejectsInvalidPolicy(t *testing.T) {
	base := func(body string) string {
		return "slot_defaults:\n  worldId: {role: world, ask: never}\ncapabilities:\n" + body
	}
	for name, doc := range map[string]string{
		"unknown tier":    base("  - {id: a.b, tier: R9, confirm: none}\n"),
		"unknown confirm": base("  - {id: a.b, tier: R1, confirm: maybe}\n"),
		"unknown role":    base("  - {id: a.b, tier: R1, confirm: none, slots: {x: {role: weird}}}\n"),
		"unknown ask":     base("  - {id: a.b, tier: R1, confirm: none, slots: {x: {role: text, ask: sometimes}}}\n"),
		"r0 confirm":      base("  - {id: a.b, tier: R0, confirm: once}\n"),
		"r2 no confirm":   base("  - {id: a.b, tier: R2, confirm: none}\n"),
		"r3 not typed":    base("  - {id: a.b, tier: R3, confirm: once}\n"),
		"duplicate id":    base("  - {id: a.b, tier: R1, confirm: none}\n  - {id: a.b, tier: R1, confirm: none}\n"),
		"unknown field":   base("  - {id: a.b, tier: R1, confirm: none, handler: lua}\n"),
		"bad id":          base("  - {id: 'A B', tier: R1, confirm: none}\n"),
		"self prefer":     base("  - {id: a.b, tier: R1, confirm: none, prefer: a.b}\n"),
		"model internal":  base("  - {id: a.b, tier: R1, confirm: none, slots: {x: {role: internal, model: true}}}\n"),
		"ask internal":    base("  - {id: a.b, tier: R1, confirm: none, slots: {x: {role: presentation, ask: if_missing}}}\n"),
		"action tier":     base("  - {id: a.b, tier: R1, confirm: none, actions: {x: {tier: R3, confirm: none}}}\n"),
		"bad action key":  base("  - {id: a.b, tier: R1, confirm: none, actions: {'X Y': {}}}\n"),
		"empty alias":     base("  - {id: a.b, tier: R1, confirm: none, aliases: ['  ']}\n"),
		"bad default":     "slot_defaults:\n  worldId: {role: nowhere}\ncapabilities: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted invalid catalog: %v", err)
			}
		})
	}
}

func TestParseDefaultsAndDedup(t *testing.T) {
	c, err := Parse([]byte(strings.Join([]string{
		"slot_defaults:",
		"  feedback: {role: presentation}",
		"capabilities:",
		"  - id: x.y",
		"    aliases: [a, a, b]",
		"    tier: R2",
		"    confirm: once",
		"    slots: {n: {role: quantity}}",
		"    actions: {k: {aliases: [c]}}",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := c.Lookup("x.y")
	if len(e.Aliases) != 2 {
		t.Fatal(e.Aliases)
	}
	if s, _ := e.Slot("n"); s.Ask != "if_missing" || !s.Model {
		t.Fatal(s)
	}
	if s, _ := e.Slot("feedback"); s.Ask != "never" || s.Model {
		t.Fatal(s)
	}
	if a := e.Actions["k"]; a.Tier != TierRoomWide || a.Confirm != ConfirmOnce {
		t.Fatal(a)
	}
	if _, ok := e.Slot("missing"); ok {
		t.Fatal("unknown slot resolved")
	}
}
