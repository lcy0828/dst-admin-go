package mod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const (
	modInfoLocale          = "zh"
	embeddedLuaTimeout     = 3 * time.Second
	externalLuaTimeout     = 5 * time.Second
	maxLuaTableRenderDepth = 100
)

type modInfoRenderMethod string

const (
	modInfoRenderEmbedded modInfoRenderMethod = "gopher-lua"
	modInfoRenderExternal modInfoRenderMethod = "external-lua"
)

type modInfoFallback func(modInfoPath string) ([]byte, error)

// renderModInfo uses the embedded interpreter first and preserves the existing
// external Lua parser as a compatibility fallback.
func renderModInfo(modID, modInfoPath string) ([]byte, modInfoRenderMethod, error) {
	return renderModInfoWithFallback(modID, modInfoPath, renderModInfoExternal)
}

func renderModInfoWithFallback(modID, modInfoPath string, fallback modInfoFallback) ([]byte, modInfoRenderMethod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), embeddedLuaTimeout)
	defer cancel()

	result, err := renderModInfoEmbedded(ctx, modID, modInfoPath)
	if err == nil {
		return result, modInfoRenderEmbedded, nil
	}

	embeddedErr := err
	log.Printf("%s 内嵌Lua解析失败，尝试外部Lua fallback - 模组ID: %s, 错误: %v", LogPrefix, modID, embeddedErr)
	result, err = fallback(modInfoPath)
	if err != nil {
		return nil, "", fmt.Errorf("embedded Lua failed: %v; external Lua fallback failed: %w", embeddedErr, err)
	}
	return result, modInfoRenderExternal, nil
}

func isWorkshopModID(modID string) bool {
	if modID == "" {
		return false
	}
	for _, char := range modID {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func modInfoErrorJSON(message string) string {
	encoded, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return `{"error":"modinfo error"}`
	}
	return string(encoded)
}

func renderModInfoEmbedded(ctx context.Context, modID, modInfoPath string) ([]byte, error) {
	absPath, err := filepath.Abs(modInfoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve modinfo path: %w", err)
	}
	if info, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("stat modinfo: %w", err)
	} else if info.IsDir() {
		return nil, errors.New("modinfo path is a directory")
	}

	L := lua.NewState()
	defer L.Close()
	L.SetContext(ctx)

	builtins := luaGlobalNames(L)
	configureLuaModulePath(L, filepath.Dir(absPath))
	installDSTModInfoGlobals(L, modID, modInfoLocale)

	if err := L.DoFile(absPath); err != nil {
		return nil, fmt.Errorf("execute modinfo: %w", err)
	}

	result := make(map[string]interface{})
	visiting := make(map[*lua.LTable]bool)
	global := L.Get(lua.GlobalsIndex).(*lua.LTable)
	var conversionErr error
	global.ForEach(func(key, value lua.LValue) {
		if conversionErr != nil || builtins[key.String()] || isInjectedModInfoGlobal(key.String()) || value.Type() == lua.LTFunction {
			return
		}

		converted, err := luaValueToJSON(value, visiting, 0)
		if err != nil {
			conversionErr = fmt.Errorf("convert global %q: %w", key.String(), err)
			return
		}
		result[key.String()] = converted
	})
	if conversionErr != nil {
		return nil, conversionErr
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode modinfo JSON: %w", err)
	}
	return validateModInfoJSON(encoded)
}

func configureLuaModulePath(L *lua.LState, modDir string) {
	packageTable, ok := L.GetGlobal("package").(*lua.LTable)
	if !ok {
		return
	}

	currentPath := packageTable.RawGetString("path").String()
	localPath := filepath.ToSlash(filepath.Join(modDir, "?.lua")) + ";" +
		filepath.ToSlash(filepath.Join(modDir, "?", "init.lua"))
	packageTable.RawSetString("path", lua.LString(localPath+";"+currentPath))
}

func installDSTModInfoGlobals(L *lua.LState, modID, locale string) {
	L.SetGlobal("locale", lua.LString(locale))
	L.SetGlobal("folder_name", lua.LString("workshop-"+modID))
	L.SetGlobal("ChooseTranslationTable", L.NewFunction(func(L *lua.LState) int {
		table := L.CheckTable(1)
		translated := table.RawGetString(locale)
		if translated == lua.LNil {
			translated = table.RawGetInt(1)
		}
		L.Push(translated)
		return 1
	}))
}

func isInjectedModInfoGlobal(name string) bool {
	switch name {
	case "locale", "folder_name", "ChooseTranslationTable":
		return true
	default:
		return false
	}
}

func luaGlobalNames(L *lua.LState) map[string]bool {
	names := make(map[string]bool)
	global := L.Get(lua.GlobalsIndex).(*lua.LTable)
	global.ForEach(func(key, _ lua.LValue) {
		names[key.String()] = true
	})
	return names
}

func luaValueToJSON(value lua.LValue, visiting map[*lua.LTable]bool, depth int) (interface{}, error) {
	if depth > maxLuaTableRenderDepth {
		return nil, fmt.Errorf("Lua table nesting exceeds %d levels", maxLuaTableRenderDepth)
	}

	switch value.Type() {
	case lua.LTNil:
		return nil, nil
	case lua.LTBool:
		return bool(value.(lua.LBool)), nil
	case lua.LTNumber:
		return float64(value.(lua.LNumber)), nil
	case lua.LTString:
		return string(value.(lua.LString)), nil
	case lua.LTFunction:
		return nil, nil
	case lua.LTTable:
		table := value.(*lua.LTable)
		if visiting[table] {
			return nil, errors.New("cyclic Lua table")
		}
		visiting[table] = true
		defer delete(visiting, table)

		if length, ok := luaArrayLength(table); ok && length > 0 {
			array := make([]interface{}, length)
			for index := 1; index <= length; index++ {
				converted, err := luaValueToJSON(table.RawGetInt(index), visiting, depth+1)
				if err != nil {
					return nil, err
				}
				array[index-1] = converted
			}
			return array, nil
		}

		mapped := make(map[string]interface{})
		var conversionErr error
		table.ForEach(func(key, item lua.LValue) {
			if conversionErr != nil {
				return
			}
			converted, err := luaValueToJSON(item, visiting, depth+1)
			if err != nil {
				conversionErr = err
				return
			}
			mapped[luaTableKeyString(key)] = converted
		})
		return mapped, conversionErr
	default:
		return nil, nil
	}
}

func luaArrayLength(table *lua.LTable) (int, bool) {
	count := 0
	maxIndex := 0
	sequential := true
	table.ForEach(func(key, _ lua.LValue) {
		index, ok := key.(lua.LNumber)
		if !ok || index < 1 || index != lua.LNumber(int(index)) {
			sequential = false
			return
		}
		count++
		if int(index) > maxIndex {
			maxIndex = int(index)
		}
	})
	return maxIndex, sequential && count == maxIndex
}

func luaTableKeyString(key lua.LValue) string {
	switch key.Type() {
	case lua.LTString:
		return string(key.(lua.LString))
	case lua.LTNumber:
		return strconv.FormatFloat(float64(key.(lua.LNumber)), 'g', -1, 64)
	default:
		return key.String()
	}
}

func renderModInfoExternal(modInfoPath string) ([]byte, error) {
	modInfoAbs, err := filepath.Abs(modInfoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve modinfo path: %w", err)
	}
	modGetInfoAbs, err := filepath.Abs(filepath.Join(luaShPath, "modgetinfo.lua"))
	if err != nil {
		return nil, fmt.Errorf("resolve external Lua helper: %w", err)
	}
	if _, err := os.Stat(modGetInfoAbs); err != nil {
		return nil, fmt.Errorf("stat external Lua helper: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), externalLuaTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lua", modGetInfoAbs)
	cmd.Dir = filepath.Dir(modInfoAbs)
	cmd.Env = append(os.Environ(),
		"LUA_PATH="+luaSourceSearchPath(filepath.Dir(modInfoAbs), luaShPath)+";;",
		"LUA_CPATH="+luaCModuleSearchPath(luaShPath)+";;",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("external Lua timed out after %s", externalLuaTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("run external Lua: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return validateModInfoJSON(output)
}

func luaSourceSearchPath(modDir, helperDir string) string {
	return filepath.ToSlash(filepath.Join(modDir, "?.lua")) + ";" +
		filepath.ToSlash(filepath.Join(modDir, "?", "init.lua")) + ";" +
		filepath.ToSlash(filepath.Join(helperDir, "?.lua"))
}

func luaCModuleSearchPath(helperDir string) string {
	return filepath.ToSlash(filepath.Join(helperDir, "?.so")) + ";" +
		filepath.ToSlash(filepath.Join(helperDir, "luaclib", "?.so"))
}

func validateModInfoJSON(data []byte) ([]byte, error) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil, errors.New("modinfo renderer returned no data")
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid modinfo JSON: %w", err)
	}
	if len(result) == 0 {
		return nil, errors.New("modinfo renderer returned an empty object")
	}
	return data, nil
}

func cacheModInfo(modID, sourcePath string) (string, error) {
	cacheDir := filepath.Join(luaShPath, "temp", modID)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create modinfo cache: %w", err)
	}

	cachePath := filepath.Join(cacheDir, "modinfo.lua")
	if err := copyFile(sourcePath, cachePath); err != nil {
		return "", fmt.Errorf("cache modinfo: %w", err)
	}
	return cachePath, nil
}

func copyFile(sourcePath, destinationPath string) error {
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	destinationAbs, err := filepath.Abs(destinationPath)
	if err != nil {
		return err
	}
	if sourceAbs == destinationAbs {
		return nil
	}

	source, err := os.Open(sourceAbs)
	if err != nil {
		return err
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(destinationAbs), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destinationAbs), ".modinfo-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destinationAbs)
}
