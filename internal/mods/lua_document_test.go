package mods

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModOverrideRoundTripPreservesUnknownValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modoverrides.lua")
	source := `return {
  ["workshop-123"] = {
    enabled = true,
    configuration_options = { known = "before", mystery = { nested = 42, flags = {true, false} } },
    future_field = "keep-me",
  },
  ["manual-entry"] = {custom = "keep"},
}`
	if err := os.WriteFile(path, []byte(source), 0640); err != nil {
		t.Fatal(err)
	}
	document, err := loadModOverride(path)
	if err != nil {
		t.Fatal(err)
	}
	if !document.exists {
		t.Fatal("existing document was marked missing")
	}
	entry, ok := document.mod("123")
	if !ok {
		t.Fatal("Mod entry missing")
	}
	options, _ := entry.stringEntry("configuration_options")
	options.setStringEntry("known", stringNode("after"))
	rendered, err := renderModOverride(document)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := parseLuaTable(rendered, "roundtrip.lua")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"manual-entry", "future_field", "mystery", "flags"} {
		if !strings.Contains(string(rendered), expected) {
			t.Fatalf("unknown value %q was lost:\n%s", expected, rendered)
		}
	}
	if reparsed == nil {
		t.Fatal("rendered document is not parseable")
	}
}

func TestMissingModOverrideRetainsMissingSnapshotState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modoverrides.lua")
	document, err := loadModOverride(path)
	if err != nil {
		t.Fatal(err)
	}
	if document.exists {
		t.Fatal("missing document was marked existing")
	}
	ensureModEntry(&document, "123", true)
	next, err := renderModOverride(document)
	if err != nil {
		t.Fatal(err)
	}
	mutation := fileMutation{path: path, data: next, previous: fileSnapshot{data: document.data, mode: document.mode, exists: document.exists}}
	if mutation.previous.exists {
		t.Fatal("rollback would preserve a file that did not originally exist")
	}
}
