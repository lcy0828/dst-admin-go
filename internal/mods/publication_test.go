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

func TestMutateModOverrideCanPreserveEnabledWhileConfiguring(t *testing.T) {
	content := []byte(`return {["workshop-100"]={enabled=false,configuration_options={mode="easy"}}}`)
	snapshot, err := InspectModOverride(content)
	if err != nil {
		t.Fatal(err)
	}
	result, err := MutateModOverride(content, OverrideMutation{
		Action: OverrideActionConfigure, ModID: "100", Enabled: true, PreserveEnabled: true, ExpectedRevision: snapshot.Revision,
		Fields: []ConfigField{{Key: "mode", Label: "模式", Type: "string", Options: []ConfigOption{{Value: "easy"}, {Value: "hard"}}}},
		Patch:  map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mods[0].Enabled || !strings.Contains(string(result.Content), `mode = "hard"`) {
		t.Fatalf("configuration changed enabled state: mods=%#v content=%s", result.Mods, result.Content)
	}
	if len(result.Changes) != 1 || result.Changes[0].Path != "configuration_options.mode" {
		t.Fatalf("unexpected changes: %#v", result.Changes)
	}
}

func TestSharedConfigurationCopiesUnknownSourceOptionsAndPreservesWorldFields(t *testing.T) {
	content := []byte(`return {custom="keep",["workshop-100"]={enabled=false,custom="target",configuration_options={mode="easy",target_only=1}},["workshop-200"]={enabled=true}}`)
	source := []byte(`return {["workshop-100"]={enabled=true,configuration_options={mode="easy",source_only={nested=42}}}}`)
	snapshot, err := InspectModOverride(content)
	if err != nil {
		t.Fatal(err)
	}
	result, err := MutateModOverride(content, OverrideMutation{
		Action: OverrideActionConfigure, ModID: "100", PreserveEnabled: true,
		ExpectedRevision: snapshot.Revision, ConfigurationSource: source,
		Fields: []ConfigField{{Key: "mode", Type: "string", Options: []ConfigOption{{Value: "easy"}, {Value: "hard"}}}},
		Patch:  map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`custom = "keep"`, `custom = "target"`, `workshop-200`, `mode = "hard"`, `source_only`, `nested = 42`} {
		if !strings.Contains(string(result.Content), value) {
			t.Fatalf("lost %q: %s", value, result.Content)
		}
	}
	if strings.Contains(string(result.Content), "target_only") || result.Mods[0].Enabled {
		t.Fatalf("incomplete unification: %s", result.Content)
	}
	if !strings.Contains(string(source), `mode="easy"`) {
		t.Fatal("source bytes mutated")
	}
}
