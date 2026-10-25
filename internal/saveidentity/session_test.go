package saveidentity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveIndexDataFormatsAndLegacySlotSelection(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		legacy, invalid  bool
	}{
		{name: "plain", data: `return {version=5,session_id="ABC",world={}}`, want: "ABC"},
		{name: "Klei header", data: "KLEI     1 return {session_id='ABC'}\x00", want: "ABC"},
		{name: "header nul", data: "KLEI     1\x00return {session_id='ABC'}", want: "ABC"},
		{name: "empty world", data: `return {world={}}`},
		{name: "nil session", data: `return {session_id=nil}`},
		{name: "shared references", data: `local t={session_id="ABC",world={}};t.world.settings=t.world;return t`, want: "ABC"},
		{name: "assigned identity", data: `local t={session_id="OLD"};t.session_id="NEW";return t`, want: "NEW"},
		{name: "aliased identity", data: `local t={session_id="OLD"};t.alias=t;t.alias.session_id="NEW";return t`, want: "NEW"},
		{name: "scalar aliases capture their original value", data: `local session="ORIGINAL";local t={session_id=session};session="CHANGED";return t`, want: "ORIGINAL"},
		{name: "table reassignment", data: `local a={session_id="ORIGINAL"};local t={alias=a};a={session_id="CHANGED"};return t.alias`, want: "ORIGINAL"},
		{name: "legacy first slot", legacy: true, data: `return {last_used_slot=2,slots={{session_id="FIRST"},{session_id="OTHER"}}}`, want: "FIRST"},
		{name: "legacy keyed slot", legacy: true, data: `return {slots={[1]={session_id="FIRST"},[2]={session_id="OTHER"}}}`, want: "FIRST"},
		{name: "missing legacy slots", legacy: true, data: `return {}`, invalid: true},
		{name: "malformed", data: `return {`, invalid: true},
		{name: "dynamic data", data: `local t={session_id="ABC"};setmetatable(t,{});return t`, invalid: true},
		{name: "nested call", data: `return {session_id="ABC",data=os.execute("bad")}`, invalid: true},
		{name: "nil global", data: `return {session_id=unknown}`},
		{name: "traversal", data: `return {session_id="../ABC"}`, invalid: true},
		{name: "compressed header", data: `KLEI0001compressed`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sessionFromIndex([]byte(tc.data), tc.legacy)
			if got != tc.want || errors.Is(err, ErrInvalidIndex) != tc.invalid {
				t.Fatalf("session=%q error=%v", got, err)
			}
		})
	}
}

func TestCurrentSessionFollowsIndexAndInvalidatesCache(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("save/session/ACTIVE/0000000001", "active")
	write("save/session/UNUSED/0000000001", "unused")
	write("save/shardindex", `return {session_id="ACTIVE"}`)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "save/session/UNUSED"), future, future); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if got, err := CurrentSession(root); err != nil || got != "ACTIVE" {
			t.Fatalf("session=%q err=%v", got, err)
		}
	}
	// Atomic replacement is detected even if size and mtime happen to match.
	path := filepath.Join(root, "save/shardindex")
	old, _ := os.Stat(path)
	write("save/next", `return {session_id="UNUSED"}`)
	if err := os.Chtimes(filepath.Join(root, "save/next"), old.ModTime(), old.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "save/next"), path); err != nil {
		t.Fatal(err)
	}
	if got, err := CurrentSession(root); err != nil || got != "UNUSED" {
		t.Fatalf("cached old index: %q %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentSession(root); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("ambiguous session accepted: %v", err)
	}
	write("save/saveindex", `return {slots={{session_id="ACTIVE"}}}`)
	write("save/shardindex", `broken`)
	if got, err := CurrentSession(root); err != nil || got != "ACTIVE" {
		t.Fatalf("legacy fallback: %q %v", got, err)
	}
	write("save/shardindex", `return {}`)
	if got, err := CurrentSession(root); err != nil || got != "" {
		t.Fatalf("empty index must not guess: %q %v", got, err)
	}
}

func TestIndexRejectsUnsafeOrOversizedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "save"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "save/shardindex")
	outside := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(outside, []byte(`return {session_id="OTHER"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IndexedSession(root); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("symlink accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxIndexBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IndexedSession(root); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("oversize accepted: %v", err)
	}
}
