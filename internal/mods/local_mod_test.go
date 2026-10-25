package mods

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalOverrideMutationPreservesMixedConfiguration(t *testing.T) {
	content := []byte(`return {custom="keep",fishdried_custom={enabled=true,configuration_options={mode="easy",unknown={a=42}}},["123"]={enabled=false},["workshop-123"]={enabled=true,configuration_options={value=7}}}`)
	snapshot, err := InspectModOverride(content)
	if err != nil || len(snapshot.Mods) != 3 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	result, err := MutateModOverride(content, OverrideMutation{Action: OverrideActionConfigure, ModID: "fishdried_custom", PreserveEnabled: true, ExpectedRevision: snapshot.Revision, Patch: map[string]json.RawMessage{"mode": json.RawMessage(`"hard"`)}, Fields: []ConfigField{{Key: "mode", Type: "string", Options: []ConfigOption{{Value: "easy"}, {Value: "hard"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fishdried_custom", "workshop-123", `custom = "keep"`, `mode = "hard"`, "a = 42"} {
		if !strings.Contains(string(result.Content), want) {
			t.Fatalf("lost %q: %s", want, result.Content)
		}
	}
	if _, err := MutateModOverride(result.Content, OverrideMutation{Action: OverrideActionEnable, ModID: "local:fishdried_custom", ExpectedRevision: snapshot.Revision}); err == nil {
		t.Fatal("stale revision accepted")
	}
	disabled, err := MutateModOverride(result.Content, OverrideMutation{Action: OverrideActionEnable, ModID: "local:fishdried_custom", ExpectedRevision: result.NextRevision, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	next, _ := InspectModOverride(disabled.Content)
	for _, item := range next.Mods {
		if item.ModID == "local:fishdried_custom" && item.Enabled {
			t.Fatal("local Mod remained enabled")
		}
	}
	workshop, err := MutateModOverride(disabled.Content, OverrideMutation{Action: OverrideActionEnable, ModID: "123", ExpectedRevision: next.Revision, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fishdried_custom", `mode = "hard"`, "a = 42", `custom = "keep"`} {
		if !strings.Contains(string(workshop.Content), want) {
			t.Fatalf("Workshop edit lost local configuration %s: %s", want, workshop.Content)
		}
	}
}

func TestLocalModInfoUsesRealFolderName(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "mods", "fishdried_custom")
	if err := os.MkdirAll(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "modinfo.lua"), []byte(`name=folder_name; version="1.2"; configuration_options={{name="mode",default="easy"}}`), 0640); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), "", root, "local:fishdried_custom")
	if err != nil || parsed.Values["name"] != "fishdried_custom" {
		t.Fatalf("%#v %v", parsed, err)
	}
	if err := os.Symlink(directory, filepath.Join(root, "mods", "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), "", root, "local:escape"); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Rename(filepath.Join(directory, "modinfo.lua"), filepath.Join(directory, "original.lua")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.lua", filepath.Join(directory, "modinfo.lua")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), "", root, "local:fishdried_custom"); err == nil {
		t.Fatal("local modinfo symlink accepted")
	}
}

func TestMixedLocalListHasEightModsAndNeverLosesUnknownEntries(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	content := `return {fishdried_custom={enabled=true},["workshop-1"]={enabled=true},["workshop-2"]={enabled=true},["workshop-3"]={enabled=false},["workshop-4"]={enabled=true},["workshop-5"]={enabled=true},["workshop-6"]={enabled=true},["workshop-7"]={enabled=true},operator_value={keep=1}}`
	list, err := service.ListFromOverrides(context.Background(), "room-1", map[string][]byte{"world-1": []byte(content)})
	if err != nil || list.Total != 8 || len(list.Items) != 8 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	for _, item := range list.Items {
		if item.ID == "local:fishdried_custom" && (item.Source != "local" || item.DirectoryName != "fishdried_custom" || !item.Enabled) {
			t.Fatal(item)
		}
	}
}

func TestUnsafeOverrideShapesAreRejectedWithoutExecuting(t *testing.T) {
	for _, content := range []string{
		`return {fishdried_custom={enabled=true},fishdried_custom={enabled=false}}`,
		`local t={}; t.fishdried_custom={enabled=true}; return t`,
		`return {fishdried_custom=(function() return {enabled=true} end)()}`,
		`return {fishdried_custom={enabled=true,configuration_options={mode="one",mode="two"}}}`,
	} {
		if _, err := InspectModOverride([]byte(content)); err == nil {
			t.Fatalf("accepted lossy document: %s", content)
		}
	}
	path := filepath.Join(t.TempDir(), "modoverrides.lua")
	if err := os.WriteFile(path, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadModOverride(path); err == nil {
		t.Fatal("empty existing file treated as a new document")
	}
}
