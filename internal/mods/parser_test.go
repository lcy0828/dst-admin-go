package mods

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
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

func TestDualParserUsesDiscoveredSystemLuaFallback(t *testing.T) {
	if _, err := exec.LookPath("lua"); err != nil {
		t.Skip("system Lua is not installed")
	}
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	data := `name = io and "External Lua" or error("io library unavailable")
configuration_options = {}`
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}

	result, err := NewDualParser("", "").Parse(context.Background(), "123", path)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FallbackUsed || result.Parser != "lua" || result.Values["name"] != "External Lua" {
		t.Fatalf("system Lua fallback was not used: %#v", result)
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

func TestDualParserUsesPythonLupaAfterLuaFallbackFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell test adapter is POSIX-only")
	}
	root := t.TempDir()
	modPath := filepath.Join(root, "modinfo.lua")
	if err := os.WriteFile(modPath, []byte(`error("requires DST runtime")`), 0640); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, "python-lupa-test")
	script := "#!/bin/sh\nif [ \"$1\" = \"-c\" ]; then printf 'lupa-test'; exit 0; fi\ncat >/dev/null\nprintf '%s' '{\"name\":\"Python Fallback\",\"configuration_options\":[]}'\n"
	if err := os.WriteFile(python, []byte(script), 0750); err != nil {
		t.Fatal(err)
	}
	parser := NewDualParserWithPython(filepath.Join(root, "missing-lua"), python, "")
	result, err := parser.Parse(context.Background(), "123", modPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Parser != "lua" || !result.FallbackUsed || result.Values["name"] != "Python Fallback" {
		t.Fatalf("Python fallback contract missing: %#v", result)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "Python/Lupa") {
		t.Fatalf("Python fallback was not identified: %#v", result.Warnings)
	}
}

func TestPythonFallbackRejectsLossyLuaTables(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	root := t.TempDir()
	fakeLupa := `
class FakePackage:
    path = "default-path"
    cpath = "default-cpath"

class FakeTable(dict):
    pass

class BadKey:
    pass

class Globals(dict):
    def __getattr__(self, name):
        return self[name]
    def __setattr__(self, name, value):
        self[name] = value

def lua_type(value):
    if isinstance(value, FakeTable):
        return "table"
    if callable(value):
        return "function"
    return None

class LuaRuntime:
    def __init__(self, **kwargs):
        self.values = Globals(package=FakePackage(), require=lambda name: None)
    def globals(self):
        return self.values
    def execute(self, source):
        if source.startswith("ChooseTranslationTable"):
            self.values.ChooseTranslationTable = lambda value: value
            return
        marker = source.strip()
        if marker == "cycle":
            value = FakeTable()
            value["self"] = value
        elif marker == "deep":
            value = FakeTable()
            current = value
            for _ in range(102):
                child = FakeTable()
                current["child"] = child
                current = child
        else:
            value = FakeTable()
            value[BadKey()] = "lost"
        self.values.result = value
`
	if err := os.WriteFile(filepath.Join(root, "lupa.py"), []byte(fakeLupa), 0640); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		marker string
		want   string
	}{{"cycle", "cyclic Lua table"}, {"deep", "nesting exceeds 100"}, {"bad-key", "unsupported Lua table key type"}} {
		t.Run(test.marker, func(t *testing.T) {
			modInfo := filepath.Join(t.TempDir(), "modinfo.lua")
			if err := os.WriteFile(modInfo, []byte(test.marker), 0640); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(python, "-")
			command.Stdin = strings.NewReader(pythonModInfoHelper)
			command.Env = []string{
				"PATH=" + os.Getenv("PATH"), "PYTHONPATH=" + root,
				"DST_MODINFO_PATH=" + modInfo, "DST_MODINFO_ID=123", "DST_MODINFO_LOCALE=zh",
				"LUA_PATH=;;", "LUA_CPATH=;;",
			}
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("Python helper error=%v output=%s", err, output)
			}
		})
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
	_, err := NewDualParser(binary, helperDir).parseExternalLua(context.Background(), binary, "123", modInfo)
	if err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}

func TestExternalLuaHelperRejectsLossyLuaTables(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{"cycle", `value = {}; value.self = value`, "cyclic Lua table"},
		{"deep", `value = {}; local current = value; for _ = 1, 102 do local child = {}; current.child = child; current = child end`, "nesting exceeds 100"},
		{"bad-key", `local key = {}; value = {[key] = "lost"}`, "unsupported Lua table key type"},
		{"non-finite-key", `value = {[math.huge] = "lost"}`, "unsupported non-finite Lua table key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modInfo := filepath.Join(t.TempDir(), "modinfo.lua")
			if err := os.WriteFile(modInfo, []byte(test.source), 0640); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DST_MODINFO_PATH", modInfo)
			t.Setenv("DST_MODINFO_ID", "123")
			t.Setenv("DST_MODINFO_LOCALE", "zh")
			state := lua.NewState()
			defer state.Close()
			err := state.DoString(externalModInfoHelper)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("external Lua helper error=%v", err)
			}
		})
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
