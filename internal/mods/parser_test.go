package mods

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestDualParserUsesEmbeddedParserForNormalMod(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "modinfo.lua")
	data := `name = ChooseTranslationTable({zh = "中文名", [1] = "Name"})
configuration_options = {{name = "enabled", options = {{description = "是", data = true}}, default = true}}`
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	result, err := NewDualParser("", "").Parse(context.Background(), "123", path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Parser != "go" || result.FallbackUsed || result.Values["name"] != "中文名" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestDualParserReportsExternalLuaFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	root := t.TempDir()
	modPath := filepath.Join(root, "modinfo.lua")
	if err := os.WriteFile(modPath, []byte(`error("requires DST runtime")`), 0640); err != nil {
		t.Fatal(err)
	}
	helperDir := filepath.Join(root, "helper")
	if err := os.MkdirAll(helperDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helperDir, "modgetinfo.lua"), []byte("-- test helper"), 0640); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "lua-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' '{\"name\":\"Fallback Mod\",\"configuration_options\":[]}'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	result, err := NewDualParser(binary, helperDir).Parse(context.Background(), "123", modPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Parser != "lua" || !result.FallbackUsed || result.FallbackReason == "" {
		t.Fatalf("fallback contract missing: %#v", result)
	}
	if !strings.Contains(result.Warnings[0], "fallback") {
		t.Fatalf("warning does not explain fallback: %#v", result.Warnings)
	}
}

func TestEmbeddedParserRejectsRequireTraversal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "modinfo.lua")
	if err := os.WriteFile(path, []byte(`name = require("../secret")`), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := parseModInfoEmbedded(context.Background(), "123", path)
	if err == nil || !strings.Contains(err.Error(), "unsafe module") {
		t.Fatalf("expected traversal to be rejected, got %v", err)
	}
}

func TestEmbeddedParserRejectsEmptySerializableResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	if err := os.WriteFile(path, []byte(`local internal = "only"; helper = function() end`), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := parseModInfoEmbedded(context.Background(), "123", path)
	if err == nil || !strings.Contains(err.Error(), "no serializable fields") {
		t.Fatalf("expected an empty result error, got %v", err)
	}
}

func TestExternalParserRejectsTrailingJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	root := t.TempDir()
	modInfo := filepath.Join(root, "modinfo.lua")
	helperDir := filepath.Join(root, "helper")
	if err := os.MkdirAll(helperDir, 0750); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		modInfo: `name = "fixture"`,
		filepath.Join(helperDir, "modgetinfo.lua"): "-- test helper",
	} {
		if err := os.WriteFile(path, []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(root, "lua-test")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' '{\"name\":\"first\"}{\"name\":\"second\"}'\n"), 0750); err != nil {
		t.Fatal(err)
	}
	_, err := NewDualParser(binary, helperDir).parseExternal(context.Background(), "123", modInfo)
	if err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}

func TestSafeContainedFileRejectsSymlinkedParentEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "module.lua")
	if err := os.WriteFile(outsideFile, []byte("return {}"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeContainedFile(root, filepath.Join(root, "linked", "module.lua")); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected symlinked parent escape rejection, got %v", err)
	}
}

func TestSanitizeParserErrorRedactsPathsAndSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	message := sanitizeParserErrorForPath(errors.New(path+":2: token=do-not-expose\nstack"), path)
	if strings.Contains(message, filepath.Dir(path)) || strings.Contains(message, "do-not-expose") {
		t.Fatalf("parser error was not redacted: %q", message)
	}
	if !strings.Contains(message, "modinfo.lua:2") || !strings.Contains(message, "token=<redacted>") {
		t.Fatalf("parser error lost useful context: %q", message)
	}
}

func TestDualParserFixtureDirectory(t *testing.T) {
	fixtureRoot := strings.TrimSpace(os.Getenv("DST_MODINFO_FIXTURES"))
	if fixtureRoot == "" {
		t.Skip("DST_MODINFO_FIXTURES is not set")
	}
	entries, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	parser := NewDualParser(os.Getenv("DST_MODINFO_LUA_BINARY"), os.Getenv("DST_MODINFO_HELPER_DIR"))
	forceExternal := os.Getenv("DST_MODINFO_FORCE_EXTERNAL") == "1"
	parsed, embedded, fallback := 0, 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		modInfoPath := filepath.Join(fixtureRoot, entry.Name(), "modinfo.lua")
		if _, err := os.Stat(modInfoPath); err != nil {
			continue
		}
		parsed++
		t.Run(entry.Name(), func(t *testing.T) {
			var result ParserResult
			var err error
			if forceExternal {
				var values map[string]interface{}
				values, err = parser.parseExternal(context.Background(), entry.Name(), modInfoPath)
				result = ParserResult{Values: values, Parser: "lua", FallbackUsed: true}
			} else {
				result, err = parser.Parse(context.Background(), entry.Name(), modInfoPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Values) == 0 {
				t.Fatal("parser returned no fields")
			}
			switch result.Parser {
			case "go":
				embedded++
			case "lua":
				fallback++
			default:
				t.Fatalf("unknown parser %q", result.Parser)
			}
		})
	}
	if parsed == 0 {
		t.Fatalf("no Mod fixture directories found in %q", fixtureRoot)
	}
	t.Logf("compatibility matrix: total=%d embedded=%d fallback=%d", parsed, embedded, fallback)
}
