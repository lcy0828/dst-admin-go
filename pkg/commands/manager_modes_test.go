package commands

import (
	"strings"
	"testing"

	"dont/pkg/commands/types"
)

func TestStoredModeBuiltinUsesCurrentImplementationWithoutChangingCustomCommands(t *testing.T) {
	old := &types.Command{ID: "godmode_player", IsBuiltin: true, Script: "old health setter"}
	fixed := managedModeCompatibility(old)
	if fixed == old || !strings.Contains(fixed.Script, `m.Set(c_select("{1}"),"invincible",{2})`) {
		t.Fatalf("stored mode still bypasses canonical implementation: %#v", fixed)
	}
	if old.Script != "old health setter" {
		t.Fatal("persistent source was overwritten")
	}
	for _, other := range []*types.Command{
		{ID: "godmode_player", IsBuiltin: false, Script: "custom"},
		{ID: "save_world", IsBuiltin: true, Script: "unchanged"},
	} {
		if managedModeCompatibility(other) != other {
			t.Fatal("unrelated or custom command changed")
		}
	}
}
