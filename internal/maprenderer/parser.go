package maprenderer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

const (
	worldUnitsPerTile  = 4
	maxLuaDepth        = 12
	maxPropertyEntries = 4096
	maxPropertyString  = 1024 * 1024
	maxFeatureCount    = 500000
)

func Parse(ctx context.Context, source []byte) (ParsedSave, error) {
	state := lua.NewState(lua.Options{
		SkipOpenLibs: true, CallStackSize: 256, RegistrySize: 8192,
		RegistryMaxSize: 1024 * 1024, MinimizeStackMemory: true,
	})
	defer state.Close()
	state.SetContext(ctx)
	lua.OpenBase(state)
	lua.OpenTable(state)
	lua.OpenString(state)
	lua.OpenMath(state)
	for _, name := range []string{"dofile", "loadfile", "load", "loadstring", "print", "require", "collectgarbage"} {
		state.SetGlobal(name, lua.LNil)
	}
	function, err := state.Load(strings.NewReader(string(source)), "session")
	if err != nil {
		return ParsedSave{}, fmt.Errorf("compile session Lua: %w", err)
	}
	state.Push(function)
	if err := state.PCall(0, 1, nil); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ParsedSave{}, errors.New("session Lua evaluation timed out")
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return ParsedSave{}, context.Canceled
		}
		return ParsedSave{}, fmt.Errorf("evaluate session Lua: %w", err)
	}
	root, ok := state.Get(-1).(*lua.LTable)
	if !ok {
		return ParsedSave{}, errors.New("session Lua must return a table")
	}
	return parseRoot(root)
}

func parseRoot(root *lua.LTable) (ParsedSave, error) {
	mapTable, ok := tableField(root, "map")
	if !ok {
		return ParsedSave{}, errors.New("session does not contain a map table")
	}
	width, ok := positiveIntField(mapTable, "width")
	if !ok {
		return ParsedSave{}, errors.New("session map width is invalid")
	}
	height, ok := positiveIntField(mapTable, "height")
	if !ok {
		return ParsedSave{}, errors.New("session map height is invalid")
	}
	if width > 8192 || height > 8192 || int64(width)*int64(height) > 16*1024*1024 {
		return ParsedSave{}, fmt.Errorf("session map dimensions %dx%d exceed renderer limits", width, height)
	}
	tilesValue, ok := mapTable.RawGetString("tiles").(lua.LString)
	if !ok || len(tilesValue) == 0 {
		return ParsedSave{}, errors.New("session map tiles are missing")
	}
	tiles, err := decodeTiles(string(tilesValue), width, height)
	if err != nil {
		return ParsedSave{}, err
	}
	warnings := make([]string, 0)
	features, featureWarnings := extractFeatures(root, width, height)
	warnings = append(warnings, featureWarnings...)
	tileNames := extractTileNames(mapTable)
	roads, roadWarnings := extractRoads(mapTable)
	warnings = append(warnings, roadWarnings...)
	return ParsedSave{
		TileWidth: width, TileHeight: height, TileIDs: tiles, TileNames: tileNames, Roads: roads, Features: features,
		WorldState: extractWorldState(root, &warnings), Warnings: warnings,
	}, nil
}

func extractTileNames(mapTable *lua.LTable) map[uint16]string {
	result := make(map[uint16]string)
	worldTileMap, ok := tableField(mapTable, "world_tile_map")
	if !ok {
		return result
	}
	worldTileMap.ForEach(func(nameValue, idValue lua.LValue) {
		name, nameOK := nameValue.(lua.LString)
		id, idOK := idValue.(lua.LNumber)
		if nameOK && idOK && id >= 0 && id <= 65535 && id == lua.LNumber(uint16(id)) {
			result[uint16(id)] = string(name)
		}
	})
	return result
}

func extractRoads(mapTable *lua.LTable) ([]Road, []string) {
	const maxRoads = 65536
	const maxRoadPoints = 1000000
	roadsTable, ok := tableField(mapTable, "roads")
	if !ok {
		return []Road{}, nil
	}
	result := make([]Road, 0, min(roadsTable.Len(), maxRoads))
	warnings := make([]string, 0)
	pointCount := 0
	roadsTable.ForEach(func(_, roadValue lua.LValue) {
		if len(result) >= maxRoads || pointCount >= maxRoadPoints {
			return
		}
		roadTable, ok := roadValue.(*lua.LTable)
		if !ok || roadTable.Len() < 3 {
			return
		}
		kindValue, ok := roadTable.RawGetInt(1).(lua.LNumber)
		if !ok {
			return
		}
		road := Road{Kind: int(kindValue), Points: make([]WorldPoint, 0, roadTable.Len()-1)}
		for index := 2; index <= roadTable.Len() && pointCount < maxRoadPoints; index++ {
			pointTable, ok := roadTable.RawGetInt(index).(*lua.LTable)
			if !ok {
				continue
			}
			x, xOK := pointTable.RawGetInt(1).(lua.LNumber)
			z, zOK := pointTable.RawGetInt(2).(lua.LNumber)
			if !xOK || !zOK || math.IsNaN(float64(x)) || math.IsNaN(float64(z)) || math.IsInf(float64(x), 0) || math.IsInf(float64(z), 0) {
				continue
			}
			road.Points = append(road.Points, WorldPoint{X: float64(x), Z: float64(z)})
			pointCount++
		}
		if len(road.Points) >= 2 {
			result = append(result, road)
		}
	})
	if len(result) >= maxRoads || pointCount >= maxRoadPoints {
		warnings = append(warnings, "道路数据超过渲染器限制，超出部分未渲染")
	}
	return result, warnings
}

func decodeTiles(encoded string, width, height int) ([]uint16, error) {
	compact := strings.Map(func(value rune) rune {
		if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
			return -1
		}
		return value
	}, encoded)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, fmt.Errorf("decode map tiles: %w", err)
	}
	count := width * height
	required := count * 2
	if len(decoded) < required {
		return nil, fmt.Errorf("map tiles are truncated: got %d bytes, need %d", len(decoded), required)
	}
	offset := len(decoded) - required
	if offset > 64 {
		return nil, fmt.Errorf("map tile header is unexpectedly large: %d bytes", offset)
	}
	if offset > 0 && !strings.HasPrefix(string(decoded), "VRSN") && !strings.HasPrefix(string(decoded), "VRSTN") {
		return nil, errors.New("map tile header is not recognized")
	}
	result := make([]uint16, count)
	for index := range result {
		position := offset + index*2
		result[index] = uint16(decoded[position]) | uint16(decoded[position+1])<<8
	}
	return result, nil
}

func extractFeatures(root *lua.LTable, width, height int) ([]Feature, []string) {
	ents, ok := tableField(root, "ents")
	if !ok {
		return []Feature{}, nil
	}
	features := make([]Feature, 0)
	warnings := make([]string, 0)
	ents.ForEach(func(prefabValue, collectionValue lua.LValue) {
		if len(features) >= maxFeatureCount {
			return
		}
		prefab, ok := prefabValue.(lua.LString)
		collection, tableOK := collectionValue.(*lua.LTable)
		if !ok || !tableOK {
			return
		}
		name := string(prefab)
		entryIndex := 0
		collection.ForEach(func(_, entryValue lua.LValue) {
			if len(features) >= maxFeatureCount {
				return
			}
			entryIndex++
			entry, ok := entryValue.(*lua.LTable)
			if !ok {
				return
			}
			x, xOK := numberField(entry, "x")
			z, zOK := numberField(entry, "z")
			if !xOK || !zOK {
				return
			}
			properties, conversionErr := featureProperties(entry)
			if conversionErr != nil {
				warnings = appendWarning(warnings, fmt.Sprintf("实体 %s 的部分属性因过大或不受支持而省略", name))
			}
			pixelX, pixelY := worldToPixel(x, z, width, height, pixelsPerTile(width, height))
			features = append(features, Feature{
				ID: name + ":" + strconv.Itoa(entryIndex), Prefab: name, Category: featureCategory(name),
				X: x, Z: z, PixelX: pixelX, PixelY: pixelY, Properties: properties,
			})
		})
	})
	if len(features) >= maxFeatureCount {
		warnings = appendWarning(warnings, fmt.Sprintf("实体数量超过 %d，超出部分未写入", maxFeatureCount))
	}
	sort.Slice(features, func(left, right int) bool {
		if features[left].Prefab == features[right].Prefab {
			return features[left].ID < features[right].ID
		}
		return features[left].Prefab < features[right].Prefab
	})
	return features, warnings
}

func featureProperties(entry *lua.LTable) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	entries := 0
	var conversionErr error
	entry.ForEach(func(key, value lua.LValue) {
		if conversionErr != nil || key.String() == "x" || key.String() == "z" {
			return
		}
		converted, err := luaJSONValue(value, make(map[*lua.LTable]bool), 0, &entries)
		if err != nil {
			conversionErr = err
			return
		}
		result[key.String()] = converted
	})
	return result, conversionErr
}

func extractWorldState(root *lua.LTable, warnings *[]string) map[string]interface{} {
	result := make(map[string]interface{})
	source := root
	if network, ok := tableField(root, "world_network"); ok {
		if persist, persistOK := tableField(network, "persistdata"); persistOK {
			source = persist
		} else {
			source = network
		}
	}
	for _, name := range []string{"clock", "seasons", "weather", "worldtemperature", "moisture", "moonstormmanager"} {
		value := source.RawGetString(name)
		if value == lua.LNil {
			continue
		}
		entries := 0
		converted, err := luaJSONValue(value, make(map[*lua.LTable]bool), 0, &entries)
		if err != nil {
			*warnings = appendWarning(*warnings, fmt.Sprintf("世界状态 %s 因过大或不受支持而省略", name))
			continue
		}
		result[name] = converted
	}
	return result
}

func luaJSONValue(value lua.LValue, visiting map[*lua.LTable]bool, depth int, entries *int) (interface{}, error) {
	if depth > maxLuaDepth || *entries > maxPropertyEntries {
		return nil, errors.New("Lua value exceeds conversion limits")
	}
	switch typed := value.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(typed), nil
	case lua.LNumber:
		number := float64(typed)
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, errors.New("Lua value contains a non-finite number")
		}
		return number, nil
	case lua.LString:
		if len(typed) > maxPropertyString {
			return nil, errors.New("Lua string exceeds conversion limits")
		}
		return string(typed), nil
	case *lua.LTable:
		if visiting[typed] {
			return nil, errors.New("Lua value contains a cycle")
		}
		visiting[typed] = true
		defer delete(visiting, typed)
		if length, array := luaArrayLength(typed); array {
			result := make([]interface{}, length)
			for index := 1; index <= length; index++ {
				*entries++
				converted, err := luaJSONValue(typed.RawGetInt(index), visiting, depth+1, entries)
				if err != nil {
					return nil, err
				}
				result[index-1] = converted
			}
			return result, nil
		}
		result := make(map[string]interface{})
		var conversionErr error
		typed.ForEach(func(key, item lua.LValue) {
			if conversionErr != nil {
				return
			}
			*entries++
			if *entries > maxPropertyEntries {
				conversionErr = errors.New("Lua table exceeds conversion limits")
				return
			}
			converted, err := luaJSONValue(item, visiting, depth+1, entries)
			if err != nil {
				conversionErr = err
				return
			}
			result[key.String()] = converted
		})
		return result, conversionErr
	default:
		return nil, fmt.Errorf("unsupported Lua value type %s", value.Type().String())
	}
}

func luaArrayLength(table *lua.LTable) (int, bool) {
	count, maximum := 0, 0
	array := true
	table.ForEach(func(key, _ lua.LValue) {
		number, ok := key.(lua.LNumber)
		if !ok || number < 1 || number != lua.LNumber(int(number)) {
			array = false
			return
		}
		count++
		maximum = max(maximum, int(number))
	})
	return maximum, array && count == maximum
}

func featureCategory(prefab string) string {
	value := strings.ToLower(prefab)
	switch {
	case value == "walrus_camp" || value == "walrus_camp_spawner":
		return "walrusCamp"
	case value == "multiplayer_portal" || value == "portal_dst" || strings.Contains(value, "spawnpoint"):
		return "spawnPoint"
	case value == "player" || strings.HasPrefix(value, "wilson"):
		return "player"
	case strings.Contains(value, "tree") || strings.Contains(value, "bush") || strings.Contains(value, "rock") || strings.Contains(value, "grass") || strings.Contains(value, "sapling") || strings.Contains(value, "berry"):
		return "resource"
	case strings.Contains(value, "house") || strings.Contains(value, "camp") || strings.Contains(value, "portal") || strings.Contains(value, "statue") || strings.Contains(value, "altar"):
		return "landmark"
	default:
		return "other"
	}
}

func tableField(table *lua.LTable, name string) (*lua.LTable, bool) {
	value, ok := table.RawGetString(name).(*lua.LTable)
	return value, ok
}

func numberField(table *lua.LTable, name string) (float64, bool) {
	value, ok := table.RawGetString(name).(lua.LNumber)
	if !ok || math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
		return 0, false
	}
	return float64(value), true
}

func positiveIntField(table *lua.LTable, name string) (int, bool) {
	value, ok := numberField(table, name)
	return int(value), ok && value > 0 && value == math.Trunc(value)
}

func appendWarning(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
