package mods

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dont/internal/modruntime"

	lua "github.com/yuin/gopher-lua"
)

const (
	embeddedParserTimeout = 3 * time.Second
	externalParserTimeout = 8 * time.Second
	maxParserOutputBytes  = int64(8 * 1024 * 1024)
	maxParserErrorBytes   = int64(16 * 1024)
)

var luaModuleNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)
var parserSecretPattern = regexp.MustCompile(`(?i)\b(api[_ -]?key|token|password|secret)\b\s*[:=]\s*[^;\s]+`)

//go:embed modinfo_fallback.lua
var externalModInfoHelper string

//go:embed modinfo_fallback.py
var pythonModInfoHelper string

type ModInfoParser interface {
	Parse(context.Context, string, string) (ParserResult, error)
}

type DualParser struct {
	LuaBinary    string
	PythonBinary string
	HelperDir    string
}

func NewDualParser(luaBinary, helperDir string) *DualParser {
	return NewDualParserWithPython(luaBinary, "", helperDir)
}

func NewDualParserWithPython(luaBinary, pythonBinary, helperDir string) *DualParser {
	return &DualParser{
		LuaBinary: strings.TrimSpace(luaBinary), PythonBinary: strings.TrimSpace(pythonBinary),
		HelperDir: strings.TrimSpace(helperDir),
	}
}

func (p *DualParser) Parse(ctx context.Context, modID, modInfoPath string) (ParserResult, error) {
	embeddedCtx, cancel := context.WithTimeout(ctx, embeddedParserTimeout)
	values, embeddedErr := parseModInfoEmbedded(embeddedCtx, modID, modInfoPath)
	cancel()
	if embeddedErr == nil {
		return ParserResult{Values: values, Parser: "go", Warnings: []string{}}, nil
	}
	values, fallbackRuntime, fallbackErr := p.parseFallback(ctx, modID, modInfoPath)
	if fallbackErr != nil {
		return ParserResult{}, fmt.Errorf(
			"modinfo compatibility parsing failed: embedded: %s; fallback: %s",
			sanitizeParserErrorForPath(embeddedErr, modInfoPath),
			sanitizeParserErrorForPath(fallbackErr, modInfoPath),
		)
	}
	reason := sanitizeParserErrorForPath(embeddedErr, modInfoPath)
	return ParserResult{
		Values: values, Parser: "lua", FallbackUsed: true, FallbackReason: reason,
		Warnings: []string{fmt.Sprintf("Go 主解析器不兼容此 Mod，已使用 %s fallback", fallbackRuntime)},
	}, nil
}

func parseModInfoEmbedded(ctx context.Context, modID, path string) (map[string]interface{}, error) {
	absPath, err := safeRegularFile(path)
	if err != nil {
		return nil, err
	}
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 128, RegistrySize: 4096, RegistryMaxSize: 65536, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(ctx)
	lua.OpenBase(state)
	lua.OpenTable(state)
	lua.OpenString(state)
	lua.OpenMath(state)
	state.SetGlobal("dofile", lua.LNil)
	state.SetGlobal("loadfile", lua.LNil)
	installSafeRequire(state, filepath.Dir(absPath))
	installModInfoGlobals(state, modID)
	builtins := luaGlobals(state)

	if err := state.DoFile(absPath); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("modinfo evaluation timed out")
		}
		return nil, fmt.Errorf("execute modinfo.lua: %w", err)
	}
	result := make(map[string]interface{})
	visiting := make(map[*lua.LTable]bool)
	global := state.Get(lua.GlobalsIndex).(*lua.LTable)
	var conversionErr error
	global.ForEach(func(key, value lua.LValue) {
		name := key.String()
		if conversionErr != nil || builtins[name] || injectedModInfoGlobal(name) || value.Type() == lua.LTFunction {
			return
		}
		converted, err := parserLuaValue(value, visiting, 0)
		if err != nil {
			conversionErr = fmt.Errorf("convert global %q: %w", name, err)
			return
		}
		result[name] = converted
	})
	if conversionErr != nil {
		return nil, conversionErr
	}
	if len(result) == 0 {
		return nil, errors.New("modinfo produced no serializable fields")
	}
	return result, nil
}

func installSafeRequire(state *lua.LState, root string) {
	loaded := make(map[string]lua.LValue)
	loading := make(map[string]bool)
	packageTable := state.NewTable()
	packageLoaded := state.NewTable()
	packageTable.RawSetString("loaded", packageLoaded)
	state.SetGlobal("package", packageTable)
	state.SetGlobal("require", state.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if value, ok := loaded[name]; ok {
			L.Push(value)
			return 1
		}
		if loading[name] {
			L.RaiseError("cyclic require for module %q", name)
			return 0
		}
		if !luaModuleNamePattern.MatchString(name) || strings.Contains(name, "..") {
			L.RaiseError("unsafe module name %q", name)
			return 0
		}
		relative := filepath.FromSlash(strings.ReplaceAll(name, ".", "/"))
		candidates := []string{filepath.Join(root, relative+".lua"), filepath.Join(root, relative, "init.lua")}
		var modulePath string
		for _, candidate := range candidates {
			resolved, err := safeContainedFile(root, candidate)
			if err == nil {
				modulePath = resolved
				break
			}
		}
		if modulePath == "" {
			L.RaiseError("module %q not found in Mod directory", name)
			return 0
		}
		loading[name] = true
		defer delete(loading, name)
		function, err := L.LoadFile(modulePath)
		if err != nil {
			L.RaiseError("load module %q: %v", name, err)
			return 0
		}
		L.Push(function)
		if err := L.PCall(0, 1, nil); err != nil {
			L.RaiseError("execute module %q: %v", name, err)
			return 0
		}
		value := L.Get(-1)
		L.Pop(1)
		if value == lua.LNil {
			value = lua.LTrue
		}
		loaded[name] = value
		packageLoaded.RawSetString(name, value)
		L.Push(value)
		return 1
	}))
}

func installModInfoGlobals(state *lua.LState, modID string) {
	state.SetGlobal("locale", lua.LString("zh"))
	state.SetGlobal("folder_name", lua.LString("workshop-"+modID))
	state.SetGlobal("ChooseTranslationTable", state.NewFunction(func(L *lua.LState) int {
		table := L.CheckTable(1)
		value := table.RawGetString("zh")
		if value == lua.LNil {
			value = table.RawGetString("zhr")
		}
		if value == lua.LNil {
			value = table.RawGetInt(1)
		}
		L.Push(value)
		return 1
	}))
	state.SetGlobal("modimport", state.GetGlobal("require"))
}

func injectedModInfoGlobal(name string) bool {
	switch name {
	case "locale", "folder_name", "ChooseTranslationTable", "modimport", "require", "package":
		return true
	default:
		return false
	}
}

func luaGlobals(state *lua.LState) map[string]bool {
	result := make(map[string]bool)
	state.Get(lua.GlobalsIndex).(*lua.LTable).ForEach(func(key, _ lua.LValue) { result[key.String()] = true })
	return result
}

func parserLuaValue(value lua.LValue, visiting map[*lua.LTable]bool, depth int) (interface{}, error) {
	if depth > maxLuaDepth {
		return nil, ErrUnsupportedLuaValue
	}
	switch value.Type() {
	case lua.LTNil:
		return nil, nil
	case lua.LTBool:
		return bool(value.(lua.LBool)), nil
	case lua.LTNumber:
		number := float64(value.(lua.LNumber))
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, ErrUnsupportedLuaValue
		}
		return number, nil
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
		if length, ok := parserArrayLength(table); ok {
			array := make([]interface{}, length)
			for index := 1; index <= length; index++ {
				converted, err := parserLuaValue(table.RawGetInt(index), visiting, depth+1)
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
			mappedKey, err := parserLuaKey(key)
			if err != nil {
				conversionErr = err
				return
			}
			if _, exists := mapped[mappedKey]; exists {
				conversionErr = fmt.Errorf("%w: duplicate JSON key %q", ErrUnsupportedLuaValue, mappedKey)
				return
			}
			converted, err := parserLuaValue(item, visiting, depth+1)
			if err != nil {
				conversionErr = err
				return
			}
			mapped[mappedKey] = converted
		})
		return mapped, conversionErr
	default:
		return nil, nil
	}
}

func parserArrayLength(table *lua.LTable) (int, bool) {
	count, maximum := 0, 0
	sequential := true
	table.ForEach(func(key, _ lua.LValue) {
		index, ok := key.(lua.LNumber)
		if !ok || index < 1 || index != lua.LNumber(int(index)) {
			sequential = false
			return
		}
		count++
		if int(index) > maximum {
			maximum = int(index)
		}
	})
	return maximum, sequential && count == maximum
}

func parserLuaKey(value lua.LValue) (string, error) {
	switch value.Type() {
	case lua.LTString:
		return string(value.(lua.LString)), nil
	case lua.LTNumber:
		number := float64(value.(lua.LNumber))
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return "", ErrUnsupportedLuaValue
		}
		return strconv.FormatFloat(number, 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("%w: table key type %s", ErrUnsupportedLuaValue, value.Type().String())
	}
}

func (p *DualParser) parseExternal(ctx context.Context, modID, modInfoPath string) (map[string]interface{}, error) {
	values, _, err := p.parseFallback(ctx, modID, modInfoPath)
	return values, err
}

func (p *DualParser) parseFallback(ctx context.Context, modID, modInfoPath string) (map[string]interface{}, string, error) {
	discovery := modruntime.Discover(p.LuaBinary, p.PythonBinary)
	if len(discovery.Runtimes) == 0 {
		return nil, "", errors.New(discovery.Diagnostic())
	}
	failures := make([]string, 0, len(discovery.Runtimes))
	for _, runtime := range discovery.Runtimes {
		var values map[string]interface{}
		var err error
		switch runtime.Kind {
		case modruntime.KindLua:
			values, err = p.parseExternalLua(ctx, runtime.Path, modID, modInfoPath)
		case modruntime.KindPythonLupa:
			values, err = p.parseExternalPython(ctx, runtime.Path, modID, modInfoPath)
		default:
			continue
		}
		if err == nil {
			return values, fallbackRuntimeLabel(runtime.Kind), nil
		}
		failures = append(failures, fmt.Sprintf("%s (%s): %v", runtime.Kind, runtime.Path, err))
	}
	return nil, "", errors.New(strings.Join(failures, "; "))
}

func fallbackRuntimeLabel(kind modruntime.Kind) string {
	if kind == modruntime.KindPythonLupa {
		return "Python/Lupa"
	}
	return "外部 Lua"
}

func (p *DualParser) parseExternalLua(ctx context.Context, binary, modID, modInfoPath string) (map[string]interface{}, error) {
	modInfoAbs, err := safeRegularFile(modInfoPath)
	if err != nil {
		return nil, err
	}
	externalCtx, cancel := context.WithTimeout(ctx, externalParserTimeout)
	defer cancel()
	command := exec.CommandContext(externalCtx, binary, "-")
	command.Dir = filepath.Dir(modInfoAbs)
	command.Stdin = strings.NewReader(externalModInfoHelper)
	command.Env = p.fallbackEnvironment(binary, modID, modInfoAbs)
	return runFallbackCommand(externalCtx, command, "external Lua")
}

func (p *DualParser) parseExternalPython(ctx context.Context, binary, modID, modInfoPath string) (map[string]interface{}, error) {
	modInfoAbs, err := safeRegularFile(modInfoPath)
	if err != nil {
		return nil, err
	}
	externalCtx, cancel := context.WithTimeout(ctx, externalParserTimeout)
	defer cancel()
	command := exec.CommandContext(externalCtx, binary, "-")
	command.Dir = filepath.Dir(modInfoAbs)
	command.Stdin = strings.NewReader(pythonModInfoHelper)
	command.Env = p.fallbackEnvironment(binary, modID, modInfoAbs)
	return runFallbackCommand(externalCtx, command, "Python/Lupa")
}

func (p *DualParser) fallbackEnvironment(binary, modID, modInfoAbs string) []string {
	modulePaths := []string{
		filepath.ToSlash(filepath.Join(filepath.Dir(modInfoAbs), "?.lua")),
		filepath.ToSlash(filepath.Join(filepath.Dir(modInfoAbs), "?", "init.lua")),
	}
	cModulePaths := []string{}
	if p.HelperDir != "" {
		modulePaths = append(modulePaths,
			filepath.ToSlash(filepath.Join(p.HelperDir, "?.lua")),
			filepath.ToSlash(filepath.Join(p.HelperDir, "?", "init.lua")),
		)
		cModulePaths = append(cModulePaths,
			filepath.ToSlash(filepath.Join(p.HelperDir, "?.so")),
			filepath.ToSlash(filepath.Join(p.HelperDir, "luaclib", "?.so")),
		)
	}
	pathDirectories := []string{filepath.Dir(binary), "/usr/local/bin", "/usr/bin", "/bin"}
	if filepath.Clean(filepath.Dir(binary)) != "/opt/homebrew/bin" {
		pathDirectories = append([]string{"/opt/homebrew/bin"}, pathDirectories...)
	}
	return []string{
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PATH=" + strings.Join(uniquePathDirectories(pathDirectories), string(os.PathListSeparator)),
		"DST_MODINFO_PATH=" + modInfoAbs,
		"DST_MODINFO_ID=" + modID,
		"DST_MODINFO_LOCALE=zh",
		"LUA_PATH=" + strings.Join(modulePaths, ";") + ";;",
		"LUA_CPATH=" + strings.Join(cModulePaths, ";") + ";;",
	}
}

func uniquePathDirectories(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = filepath.Clean(strings.TrimSpace(value))
		if value == "." || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func runFallbackCommand(externalCtx context.Context, command *exec.Cmd, label string) (map[string]interface{}, error) {
	output := &limitedBuffer{limit: maxParserOutputBytes}
	diagnostics := &limitedBuffer{limit: maxParserErrorBytes}
	command.Stdout = output
	command.Stderr = diagnostics
	if err := command.Run(); err != nil {
		if errors.Is(externalCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", label, externalParserTimeout)
		}
		if errors.Is(externalCtx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		message := strings.TrimSpace(diagnostics.String())
		if diagnostics.truncated {
			message += " [diagnostic truncated]"
		}
		if message == "" {
			return nil, fmt.Errorf("run %s: %w", label, err)
		}
		return nil, fmt.Errorf("run %s: %w: %s", label, err, message)
	}
	if output.truncated {
		return nil, fmt.Errorf("%s output exceeded 8 MiB", label)
	}
	var values map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode %s output: %w", label, err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s output contains more than one JSON value", label)
		}
		return nil, fmt.Errorf("%s output has trailing data: %w", label, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s produced an empty JSON object", label)
	}
	return values, nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.written
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	b.written += int64(len(data))
	return original, nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }

func safeRegularFile(path string) (string, error) {
	absPath, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || strings.TrimSpace(path) == "" {
		return "", errors.New("file path is required")
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxLuaBytes {
		return "", errors.New("file is unsafe or too large")
	}
	return absPath, nil
}

func safeContainedFile(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	pathResolved, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes its root")
	}
	return safeRegularFile(pathResolved)
}

func sanitizeParserError(err error) string {
	return sanitizeParserErrorForPath(err, "")
}

func sanitizeParserErrorForPath(err error, path string) string {
	if err == nil {
		return "unknown parser error"
	}
	message := err.Error()
	if path != "" {
		if absolute, resolveErr := filepath.Abs(path); resolveErr == nil {
			message = strings.ReplaceAll(message, absolute, "modinfo.lua")
			message = strings.ReplaceAll(message, filepath.Dir(absolute)+string(os.PathSeparator), "")
		}
		message = strings.ReplaceAll(message, path, "modinfo.lua")
	}
	message = parserSecretPattern.ReplaceAllString(message, "$1=<redacted>")
	message = strings.Map(func(value rune) rune {
		if value == '\n' || value == '\r' || value == '\t' {
			return ' '
		}
		if unicode.IsControl(value) {
			return -1
		}
		return value
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 500 {
		message = message[:500] + "..."
	}
	return message
}

var _ io.Writer = (*limitedBuffer)(nil)
