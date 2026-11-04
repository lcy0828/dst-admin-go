package console

import (
	"strings"
	"testing"
)

func expansionArgs(d Definition) map[string]interface{} {
	a := map[string]interface{}{}
	for _, p := range d.Parameters {
		switch {
		case p.Name == "player_id":
			a[p.Name] = "KU_EXAMPLE"
		case p.Name == "prefab" || p.Name == "recipe":
			a[p.Name] = "twigs"
		case p.Name == "destination_shard":
			a[p.Name] = "2"
		case p.Default != nil:
			a[p.Name] = p.Default
		case p.Type == "enum":
			a[p.Name] = p.Options[0]
		case p.Type == "boolean":
			a[p.Name] = false
		}
	}
	return a
}
func TestExpansionDirectoryValidationAndConfirmation(t *testing.T) {
	templates := builtinTemplates()
	for _, d := range expansionDefinitions() {
		t.Run(d.ID, func(t *testing.T) {
			a := expansionArgs(d)
			if _, err := ValidateBuiltinWorkbench(d.ID, a, false); err != nil {
				t.Fatalf("preview: %v", err)
			}
			script, err := templates[d.ID].render(a)
			if err != nil || !strings.Contains(script, "w.Expansion(") || !strings.Contains(script, "WORKBENCH_UPDATE_REQUIRED") {
				t.Fatalf("script=%s err=%v", script, err)
			}
			if guardedExpansion(d.ID) {
				a["phase"] = "execute"
				a["preview_id"] = "00000000-0000-4000-8000-000000000001"
				if _, err := ValidateBuiltinWorkbench(d.ID, a, false); err != ErrConfirmationNeeded {
					t.Fatalf("missing human confirmation: %v", err)
				}
				if _, err := ValidateBuiltinWorkbench(d.ID, a, true); err != nil {
					t.Fatalf("confirmed: %v", err)
				}
			}
			a["readOnly"] = true
			if _, err := ValidateBuiltinWorkbench(d.ID, a, true); err != ErrInvalidArguments {
				t.Fatalf("spoofed metadata: %v", err)
			}
		})
	}
}
func TestExpansionRejectsInjectionAndUnboundedArguments(t *testing.T) {
	tests := []struct {
		id, key string
		value   interface{}
	}{
		{"spawn_entity_at", "prefab", `twigs\");c_regenerateworld();--`}, {"spawn_entity_at", "skin", `skin"..os.execute("id").."`}, {"spawn_entity_at", "count", 21}, {"spawn_entity_at", "x", "0;print(1)"}, {"spawn_entity_at", "count", 1.5}, {"edit_world_tiles", "tile", "999"}, {"edit_world_tiles", "size", 6}, {"migrate_player", "destination_shard", "2/../Master"}, {"migrate_player", "portal_id", 0}, {"set_player_mode", "mode", "arbitrary_function"}, {"set_player_mode", "enabled", "true"}, {"give_character_supplies", "character", "custom_character"},
	}
	for _, tt := range tests {
		t.Run(tt.id+tt.key, func(t *testing.T) {
			d, _ := expansionDefinition(tt.id)
			a := expansionArgs(d)
			a[tt.key] = tt.value
			if _, err := ValidateBuiltinWorkbench(tt.id, a, true); err != ErrInvalidArguments {
				t.Fatalf("accepted %#v: %v", tt.value, err)
			}
		})
	}
}
func TestEveryCodeOwnedDefinitionIsVisible(t *testing.T) {
	service := newConsoleService(t, &captureSender{})
	list, err := service.DefinitionsWithError()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range list {
		if seen[d.ID] {
			t.Fatal("duplicate", d.ID)
		}
		seen[d.ID] = true
	}
	for id := range service.templates {
		if !seen[id] {
			t.Fatal("hidden", id)
		}
	}
}
