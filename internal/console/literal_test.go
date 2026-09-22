package console

import (
	"context"
	lua "github.com/yuin/gopher-lua"
	"strings"
	"testing"
)

func TestLiteralLuaDefinitionsPersistWithoutPlaceholderExpansion(t *testing.T) {
	sender := &captureSender{}
	service := newConsoleService(t, sender)
	script := `local value="{missing}";local values={value};print(values[1])`
	created, err := service.CreateDefinition(Definition{Name: "模组脚本", Category: "custom", ScriptMode: "literal", Script: script})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.Definition(created.ID)
	if err != nil || loaded.ScriptMode != "literal" || loaded.Script != script {
		t.Fatalf("%#v %v", loaded, err)
	}
	_, err = service.Execute(context.Background(), "room", "world", ExecuteRequest{CommandID: loaded.ID, Confirmation: "周末服"})
	if err != nil || !strings.Contains(sender.script, script) {
		t.Fatalf("%s %v", sender.script, err)
	}
	loaded.Script += `;print("updated")`
	updated, err := service.UpdateDefinition(loaded.ID, loaded)
	if err != nil || updated.ScriptMode != "literal" || updated.Script != loaded.Script {
		t.Fatalf("%#v %v", updated, err)
	}
}
func TestTemplateArgumentsAreExpandedOnce(t *testing.T) {
	definition := Definition{Script: `print("{first}","{second}")`, Parameters: []Parameter{{Name: "first", Type: "string"}, {Name: "second", Type: "string"}}}
	rendered, err := renderCustomDefinition(definition, map[string]interface{}{"first": "{second}", "second": "literal"})
	if err != nil || rendered != `print("{second}","literal")` {
		t.Fatalf("%q %v", rendered, err)
	}
}

func TestGiveItemRejectsNonInventoryModEntitiesAndRemovesTemporaryEntity(t *testing.T) {
	script, err := builtinTemplates()["give_item"].render(map[string]interface{}{"player_id": "KU_TEST123", "prefab": "mod_entity", "count": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	state := lua.NewState()
	defer state.Close()
	if err = state.DoString(`
 removed=false
 AllPlayers={{userid="KU_TEST123",components={inventory={GiveItem=function() error("should not give non-inventory entity") end}}}}
 SpawnPrefab=function() return {components={},Remove=function() removed=true end} end
 `); err != nil {
		t.Fatal(err)
	}
	err = state.DoString(script)
	if err == nil || !strings.Contains(err.Error(), "PREFAB_NOT_INVENTORY_ITEM") || !lua.LVAsBool(state.GetGlobal("removed")) {
		t.Fatalf("removed=%v err=%v", state.GetGlobal("removed"), err)
	}
	if err = state.DoString(`SpawnPrefab=function() return nil end`); err != nil {
		t.Fatal(err)
	}
	if err = state.DoString(script); err == nil || !strings.Contains(err.Error(), "PREFAB_NOT_FOUND") {
		t.Fatal(err)
	}
}
