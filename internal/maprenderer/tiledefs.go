package maprenderer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

type TileDefinition struct {
	Name        string
	StaticID    uint16
	Noise       string
	RenderOrder int
}

var groundEntryPattern = regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*)\s*=\s*([0-9]+)\s*,`)

func LoadTileDefinitions(ctx context.Context, assets *AssetSource) ([]TileDefinition, error) {
	tileSource, err := assets.ReadScript("tiledefs.lua")
	if err != nil {
		return nil, err
	}
	constantSource, err := assets.ReadScript("constants.lua")
	if err != nil {
		return nil, err
	}
	staticIDs := parseStaticGroundIDs(constantSource)
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 128, RegistrySize: 4096, RegistryMaxSize: 32768, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(ctx)
	lua.OpenBase(state)
	lua.OpenTable(state)
	definitions := make([]TileDefinition, 0, 128)
	tileManager := state.NewTable()
	state.SetField(tileManager, "RegisterTileRange", state.NewFunction(func(state *lua.LState) int { return 0 }))
	state.SetField(tileManager, "AddTile", state.NewFunction(func(state *lua.LState) int {
		name := state.CheckString(1)
		definition := TileDefinition{Name: name, RenderOrder: len(definitions)}
		if value, ok := staticIDs[name]; ok {
			definition.StaticID = value
		}
		if data, ok := state.Get(3).(*lua.LTable); ok {
			if value, numberOK := data.RawGetString("old_static_id").(lua.LNumber); numberOK && value >= 0 && value <= 65535 {
				definition.StaticID = uint16(value)
			}
		}
		if minimap, ok := state.Get(5).(*lua.LTable); ok {
			if value, stringOK := minimap.RawGetString("noise_texture").(lua.LString); stringOK {
				definition.Noise = string(value)
			}
		}
		definitions = append(definitions, definition)
		return 0
	}))
	state.SetGlobal("require", state.NewFunction(func(state *lua.LState) int {
		if state.CheckString(1) != "tilemanager" {
			state.RaiseError("module is not available")
		}
		state.Push(tileManager)
		return 1
	}))
	ground := state.NewTable()
	for name, value := range staticIDs {
		state.SetField(ground, name, lua.LNumber(value))
	}
	state.SetGlobal("GROUND", ground)
	for name, value := range map[string]int{
		"WORLD_TILES_LAND_START": 256, "WORLD_TILES_LAND_ONLY_END": 8448,
		"WORLD_TILES_NOISE_START": 8449, "WORLD_TILES_NOISE_END": 10497,
		"WORLD_TILES_OCEAN_START": 10498, "WORLD_TILES_OCEAN_END": 14594,
		"WORLD_TILES_IMPASSABLE_START": 14595, "WORLD_TILES_IMPASSABLE_END": 18691,
	} {
		state.SetGlobal(name, lua.LNumber(value))
	}
	if err := state.DoString(string(tileSource)); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("DST tile definition evaluation timed out")
		}
		return nil, fmt.Errorf("evaluate DST tile definitions: %w", err)
	}
	if len(definitions) == 0 {
		return nil, errors.New("DST tile definitions did not register any tiles")
	}
	return definitions, nil
}

func parseStaticGroundIDs(source []byte) map[string]uint16 {
	result := make(map[string]uint16)
	text := string(source)
	start := strings.Index(text, "GROUND =")
	end := strings.Index(text, "GROUND_NAMES")
	if start < 0 || end <= start {
		return result
	}
	for _, match := range groundEntryPattern.FindAllStringSubmatch(text[start:end], -1) {
		value, err := strconv.ParseUint(match[2], 10, 16)
		if err == nil {
			result[match[1]] = uint16(value)
		}
	}
	return result
}
