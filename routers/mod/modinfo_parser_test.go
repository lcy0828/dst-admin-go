package mod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderModInfoEmbeddedSupportsDynamicModInfo(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "labels.lua"), `return { suffix = "ready" }`)
	modInfoPath := filepath.Join(dir, "modinfo.lua")
	writeTestFile(t, modInfoPath, `
local labels = require("labels")

local function make_options()
    local options = {}
    for i = 1, 3 do
        options[#options + 1] = {
            description = "Option " .. i,
            data = i,
        }
    end
    return options
end

name = ChooseTranslationTable({ zh = "中文名称", [1] = "English Name" })
author = "tester"
version = "1.0.0"
description = folder_name .. ":" .. labels.suffix
api_version = 10
dst_compatible = true
configuration_options = {
    {
        name = "level",
        label = "Level",
        options = make_options(),
        default = 2,
    },
}
`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := renderModInfoEmbedded(ctx, "123456", modInfoPath)
	if err != nil {
		t.Fatalf("renderModInfoEmbedded() error = %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if result["name"] != "中文名称" {
		t.Fatalf("name = %v, want 中文名称", result["name"])
	}
	if result["description"] != "workshop-123456:ready" {
		t.Fatalf("description = %v, want workshop-123456:ready", result["description"])
	}

	configOptions, ok := result["configuration_options"].([]interface{})
	if !ok || len(configOptions) != 1 {
		t.Fatalf("configuration_options = %#v, want one option", result["configuration_options"])
	}
	option, ok := configOptions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("configuration option = %#v, want object", configOptions[0])
	}
	options, ok := option["options"].([]interface{})
	if !ok || len(options) != 3 {
		t.Fatalf("options = %#v, want three entries", option["options"])
	}
}

func TestRenderModInfoFallsBackAfterEmbeddedFailure(t *testing.T) {
	modInfoPath := filepath.Join(t.TempDir(), "modinfo.lua")
	writeTestFile(t, modInfoPath, `name = function(`)

	fallbackCalled := false
	output, method, err := renderModInfoWithFallback("123456", modInfoPath, func(path string) ([]byte, error) {
		fallbackCalled = true
		if path != modInfoPath {
			t.Fatalf("fallback path = %q, want %q", path, modInfoPath)
		}
		return []byte(`{"name":"fallback","configuration_options":[]}`), nil
	})
	if err != nil {
		t.Fatalf("renderModInfoWithFallback() error = %v", err)
	}
	if !fallbackCalled {
		t.Fatal("fallback was not called")
	}
	if method != modInfoRenderExternal {
		t.Fatalf("method = %q, want %q", method, modInfoRenderExternal)
	}
	if string(output) != `{"name":"fallback","configuration_options":[]}` {
		t.Fatalf("output = %s", output)
	}
}

func TestRenderModInfoDoesNotFallbackAfterEmbeddedSuccess(t *testing.T) {
	modInfoPath := filepath.Join(t.TempDir(), "modinfo.lua")
	writeTestFile(t, modInfoPath, `name = "embedded"; api_version = 10`)

	fallbackCalled := false
	_, method, err := renderModInfoWithFallback("123456", modInfoPath, func(string) ([]byte, error) {
		fallbackCalled = true
		return []byte(`{"name":"fallback"}`), nil
	})
	if err != nil {
		t.Fatalf("renderModInfoWithFallback() error = %v", err)
	}
	if fallbackCalled {
		t.Fatal("fallback was called after embedded success")
	}
	if method != modInfoRenderEmbedded {
		t.Fatalf("method = %q, want %q", method, modInfoRenderEmbedded)
	}
}

func TestRenderModInfoEmbeddedHonorsContextTimeout(t *testing.T) {
	modInfoPath := filepath.Join(t.TempDir(), "modinfo.lua")
	writeTestFile(t, modInfoPath, `while true do end`)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := renderModInfoEmbedded(ctx, "123456", modInfoPath); err == nil {
		t.Fatal("renderModInfoEmbedded() error = nil, want timeout error")
	}
}

func TestWorkshopModIDValidation(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "123456", want: true},
		{value: "", want: false},
		{value: "123 456", want: false},
		{value: "123;rm", want: false},
		{value: "workshop-123", want: false},
	}

	for _, test := range tests {
		if got := isWorkshopModID(test.value); got != test.want {
			t.Errorf("isWorkshopModID(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestRenderModInfoFixtureDirectory(t *testing.T) {
	fixtureDir := os.Getenv("DST_MODINFO_FIXTURES")
	if fixtureDir == "" {
		t.Skip("DST_MODINFO_FIXTURES is not set")
	}

	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", fixtureDir, err)
	}
	rendered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lua") {
			continue
		}
		entry := entry
		t.Run(entry.Name(), func(t *testing.T) {
			modID := strings.TrimSuffix(entry.Name(), ".lua")
			ctx, cancel := context.WithTimeout(context.Background(), embeddedLuaTimeout)
			defer cancel()
			output, err := renderModInfoEmbedded(ctx, modID, filepath.Join(fixtureDir, entry.Name()))
			if err != nil {
				t.Fatalf("renderModInfoEmbedded() error = %v", err)
			}
			if !json.Valid(output) {
				t.Fatalf("rendered output is not valid JSON: %s", output)
			}
		})
		rendered++
	}
	if rendered == 0 {
		t.Fatalf("no .lua fixtures found in %q", fixtureDir)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}
