package mods

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMutateModOverridePreservesUnknownLuaFields(t *testing.T) {
	content := []byte(`return {
  mystery = { nested = "keep" },
  ["workshop-100"] = { enabled = true, configuration_options = { old = 1 }, custom = "keep" },
}
`)
	snapshot, err := InspectModOverride(content)
	if err != nil {
		t.Fatal(err)
	}
	result, err := MutateModOverride(content, OverrideMutation{
		Action: OverrideActionAdd, ModIDs: []string{"200"}, Enabled: false, ExpectedRevision: snapshot.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(result.Content)
	for _, expected := range []string{"mystery", "nested", "custom", "workshop-100", "workshop-200"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("unknown or existing field %q was lost:\n%s", expected, rendered)
		}
	}
	if len(result.Mods) != 2 || result.Mods[1].ModID != "200" || result.Mods[1].Enabled {
		t.Fatalf("unexpected Mod state: %#v", result.Mods)
	}
}

func TestMutateModOverrideRejectsStaleRevision(t *testing.T) {
	_, err := MutateModOverride([]byte("return {}\n"), OverrideMutation{
		Action: OverrideActionAdd, ModIDs: []string{"100"}, Enabled: true, ExpectedRevision: "stale",
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
}

func TestMutateModOverrideAppliesValidatedConfigurationPatch(t *testing.T) {
	content := []byte(`return {["workshop-100"]={enabled=true,configuration_options={mode="easy",unknown="keep"}}}`)
	snapshot, err := InspectModOverride(content)
	if err != nil {
		t.Fatal(err)
	}
	result, err := MutateModOverride(content, OverrideMutation{
		Action: OverrideActionConfigure, ModID: "100", Enabled: false, ExpectedRevision: snapshot.Revision,
		Fields: []ConfigField{{Key: "mode", Label: "模式", Type: "string", Options: []ConfigOption{{Value: "easy", Label: "简单"}, {Value: "hard", Label: "困难"}}}},
		Patch:  map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), `unknown = "keep"`) || !strings.Contains(string(result.Content), `mode = "hard"`) {
		t.Fatalf("configuration patch lost values:\n%s", result.Content)
	}
	if len(result.Changes) != 2 || result.Mods[0].Enabled {
		t.Fatalf("unexpected changes: %#v mods=%#v", result.Changes, result.Mods)
	}
}
