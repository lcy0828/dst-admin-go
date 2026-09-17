package worldidentity

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

type Type string

const (
	TypeForest  Type = "forest"
	TypeCave    Type = "cave"
	TypeUnknown Type = "unknown"

	maximumOverrideBytes = 4 * 1024 * 1024
)

// ResolveType reads the world override when possible and uses only a clear
// Cave directory name as a compatibility fallback. A generic directory name
// does not prove that a world is Forest.
func ResolveType(worldPath, directoryName string) Type {
	path := filepath.Join(worldPath, "leveldataoverride.lua")
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() <= maximumOverrideBytes {
		if data, readErr := os.ReadFile(path); readErr == nil {
			if worldType := ParseType(data); worldType != TypeUnknown {
				return worldType
			}
		}
	}
	return ResolveReportedType("", directoryName)
}

func ResolveReportedType(value, directoryName string) Type {
	if strings.EqualFold(strings.TrimSpace(value), string(TypeUnknown)) {
		return TypeUnknown
	}
	if worldType := NormalizeType(value); worldType != TypeUnknown {
		return worldType
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(directoryName)), "cave") {
		return TypeCave
	}
	return TypeUnknown
}

func ParseType(data []byte) Type {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 32, RegistrySize: 1024, RegistryMaxSize: 8192, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(ctx)
	function, err := state.Load(bytes.NewReader(data), "leveldataoverride.lua")
	if err != nil {
		return TypeUnknown
	}
	state.Push(function)
	if err := state.PCall(0, 1, nil); err != nil {
		return TypeUnknown
	}
	root, ok := state.Get(-1).(*lua.LTable)
	if !ok {
		return TypeUnknown
	}
	for _, key := range []string{"location", "worldgen_id", "settings_id", "id"} {
		if worldType := NormalizeType(root.RawGetString(key).String()); worldType != TypeUnknown {
			return worldType
		}
	}
	return TypeUnknown
}

func NormalizeType(value string) Type {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch {
	case normalized == "cave", strings.Contains(normalized, "cave"):
		return TypeCave
	case normalized == "forest", strings.Contains(normalized, "survival_together"):
		return TypeForest
	default:
		return TypeUnknown
	}
}
