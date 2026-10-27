// Package modmetadata reads bounded, cached installation metadata without
// starting external interpreters or reading any game save.
package modmetadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

type Metadata struct {
	Name, Author, Version, Description, Reason string
	DSTCompatible                              *bool
	APIVersion                                 string
}
type fingerprint struct {
	Size     int64
	Modified time.Time
	Hash     [32]byte
}
type entry struct {
	value Metadata
	files map[string]fingerprint
	at    time.Time
}

var cache = struct {
	sync.Mutex
	items map[string]entry
}{items: map[string]entry{}}
var slots = make(chan struct{}, 2)
var flights sync.Map
var moduleName = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,200}$`)

const maxFile = 256 << 10
const maxTotal = 1 << 20

func safeRead(root, path string) ([]byte, fingerprint, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fingerprint{}, errors.New("unsafe metadata dependency")
	}
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fingerprint{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fingerprint{}, errors.New("symlink metadata dependency")
		}
		parent := filepath.Dir(current)
		if current == root || parent == current {
			break
		}
		current = parent
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fingerprint{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return nil, fingerprint{}, errors.New("metadata file exceeds limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fingerprint{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil || len(data) > maxFile {
		return nil, fingerprint{}, errors.New("metadata file unreadable")
	}
	return data, fingerprint{info.Size(), info.ModTime(), sha256.Sum256(data)}, nil
}

func validEntry(root string, v entry) bool {
	if len(v.files) == 0 || v.value.Reason != "" && time.Since(v.at) > 10*time.Second {
		return false
	}
	for path, old := range v.files {
		_, current, err := safeRead(root, path)
		if err != nil || old != current {
			return false
		}
	}
	return true
}

func Read(ctx context.Context, path, id, language string) Metadata {
	if language != "en" {
		language = "zh"
	}
	if ctx.Err() != nil {
		return Metadata{Reason: "metadata_timeout"}
	}
	path = filepath.Clean(path)
	root := filepath.Dir(path)
	key := path + "\x00" + id + "\x00" + language
	for {
		cache.Lock()
		cached, ok := cache.items[key]
		cache.Unlock()
		if ok && validEntry(root, cached) {
			return cached.value
		}
		done := make(chan struct{})
		old, loaded := flights.LoadOrStore(key, done)
		if loaded {
			select {
			case <-ctx.Done():
				return Metadata{Reason: "metadata_timeout"}
			case <-old.(chan struct{}):
				continue
			}
		}
		result, files := read(ctx, path, id, language)
		cache.Lock()
		if len(cache.items) >= 1024 {
			var oldest string
			var at time.Time
			for k, v := range cache.items {
				if oldest == "" || v.at.Before(at) {
					oldest, at = k, v.at
				}
			}
			delete(cache.items, oldest)
		}
		cache.items[key] = entry{result, files, time.Now()}
		cache.Unlock()
		flights.Delete(key)
		close(done)
		return result
	}
}

func text(value string, limit int) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || len(value) > limit || strings.ContainsRune(value, 0) {
		return ""
	}
	return value
}
func fields(values map[string]string) Metadata {
	result := Metadata{Name: text(values["name"], 2048), Author: text(values["author"], 2048), Version: text(values["version"], 256), Description: text(values["description"], 8192), APIVersion: text(values["api_version"], 32)}
	if values["dst_compatible"] == "true" || values["dst_compatible"] == "false" {
		value := values["dst_compatible"] == "true"
		result.DSTCompatible = &value
	}
	return result
}
func wanted(key string) bool {
	return key == "name" || key == "author" || key == "version" || key == "description" || key == "dst_compatible" || key == "api_version"
}

func read(ctx context.Context, path, id, language string) (Metadata, map[string]fingerprint) {
	root := filepath.Dir(path)
	files := map[string]fingerprint{}
	data, fp, err := safeRead(root, path)
	if err != nil {
		return Metadata{Reason: "metadata_unreadable"}, files
	}
	files[path] = fp
	// Static assignments never execute Lua. Any control flow or computed value
	// uses the bounded interpreter, preserving assignment order and translations.
	chunk, err := parse.Parse(bytes.NewReader(data), path)
	if err != nil {
		return Metadata{Reason: "metadata_parse_failed"}, files
	}
	values := map[string]string{}
	static := true
	for _, stmt := range chunk {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok {
			static = false
			break
		}
		for i, lhs := range assign.Lhs {
			name, ok := lhs.(*ast.IdentExpr)
			if !ok {
				static = false
				break
			}
			if i >= len(assign.Rhs) {
				static = false
				break
			}
			if wanted(name.Value) {
				switch v := assign.Rhs[i].(type) {
				case *ast.StringExpr:
					if name.Value != "dst_compatible" {
						values[name.Value] = v.Value
					} else {
						delete(values, name.Value)
					}
				case *ast.NumberExpr:
					if name.Value != "dst_compatible" {
						values[name.Value] = v.Value
					} else {
						delete(values, name.Value)
					}
				case *ast.TrueExpr:
					if name.Value == "dst_compatible" {
						values[name.Value] = "true"
					} else {
						delete(values, name.Value)
					}
				case *ast.FalseExpr:
					if name.Value == "dst_compatible" {
						values[name.Value] = "false"
					} else {
						delete(values, name.Value)
					}
				default:
					static = false
				}
			} else if !literal(assign.Rhs[i]) {
				static = false
				break
			}
		}
		if !static {
			break
		}
	}
	if static {
		return fields(values), files
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return Metadata{Reason: "metadata_timeout"}, files
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 64, RegistrySize: 1024, RegistryMaxSize: 16384, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(withInstructionBudget(ctx))
	lua.OpenBase(state)
	lua.OpenString(state)
	lua.OpenTable(state)
	lua.OpenMath(state)
	for _, name := range []string{"dofile", "loadfile", "loadstring", "load", "collectgarbage", "print", "_G", "getfenv", "setfenv", "rawset", "setmetatable", "getmetatable"} {
		state.SetGlobal(name, lua.LNil)
	}
	boundLibraries(state)
	state.SetGlobal("locale", lua.LString(language))
	folder := id
	if !strings.HasPrefix(id, "local:") {
		folder = "workshop-" + id
	} else {
		folder = strings.TrimPrefix(id, "local:")
	}
	state.SetGlobal("folder_name", lua.LString(folder))
	state.SetGlobal("ChooseTranslationTable", state.NewFunction(func(L *lua.LState) int {
		t := L.CheckTable(1)
		v := t.RawGetString(language)
		if v == lua.LNil && language == "zh" {
			v = t.RawGetString("zhr")
		}
		if v == lua.LNil {
			v = t.RawGetString("en")
		}
		if v == lua.LNil {
			v = t.RawGetInt(1)
		}
		L.Push(v)
		return 1
	}))
	total := len(data)
	loading := map[string]bool{}
	loaded := map[string]lua.LValue{}
	require := state.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if !moduleName.MatchString(name) || strings.Contains(name, "..") || filepath.IsAbs(name) {
			L.RaiseError("unsafe module")
			return 0
		}
		if v, ok := loaded[name]; ok {
			L.Push(v)
			return 1
		}
		if loading[name] || len(files) >= 16 {
			L.RaiseError("metadata dependency limit")
			return 0
		}
		relative := name
		if strings.HasSuffix(relative, ".lua") {
			relative = strings.TrimSuffix(relative, ".lua")
		} else {
			relative = strings.ReplaceAll(relative, ".", "/")
		}
		var body []byte
		var dependency string
		var stamp fingerprint
		var err error
		for _, candidate := range []string{filepath.Join(root, relative+".lua"), filepath.Join(root, relative, "init.lua")} {
			body, stamp, err = safeRead(root, candidate)
			if err == nil {
				dependency = candidate
				break
			}
		}
		if err != nil || total+len(body) > maxTotal {
			L.RaiseError("metadata dependency unavailable")
			return 0
		}
		total += len(body)
		files[dependency] = stamp
		loading[name] = true
		defer delete(loading, name)
		fn, err := boundedLoad(L, body, dependency)
		if err != nil {
			L.RaiseError("metadata dependency parse failed")
			return 0
		}
		L.Push(fn)
		if err := L.PCall(0, 1, nil); err != nil {
			L.RaiseError("metadata dependency failed")
			return 0
		}
		value := L.Get(-1)
		if value == lua.LNil {
			L.Pop(1)
			value = lua.LTrue
			L.Push(value)
		}
		loaded[name] = value
		return 1
	})
	state.SetGlobal("require", require)
	state.SetGlobal("modimport", require)
	fn, err := boundedLoad(state, data, path)
	if err == nil {
		state.Push(fn)
		err = state.PCall(0, 0, nil)
	}
	if err != nil {
		reason := "metadata_parse_failed"
		if state.Context().Err() != nil {
			reason = "metadata_timeout"
		}
		return Metadata{Reason: reason}, files
	}
	values = map[string]string{}
	for _, name := range []string{"name", "author", "version", "description", "dst_compatible", "api_version"} {
		v := state.GetGlobal(name)
		if name == "dst_compatible" {
			if v.Type() == lua.LTBool {
				values[name] = v.String()
			}
		} else if v.Type() == lua.LTString || v.Type() == lua.LTNumber {
			values[name] = v.String()
		}
	}
	result := fields(values)
	if result.Name == "" {
		result.Reason = "metadata_name_unavailable"
	}
	return result, files
}

func literal(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.StringExpr, *ast.NumberExpr, *ast.TrueExpr, *ast.FalseExpr, *ast.NilExpr:
		return true
	case *ast.TableExpr:
		for _, field := range v.Fields {
			if field.Key != nil && !literal(field.Key) || !literal(field.Value) {
				return false
			}
		}
		return true
	}
	return false
}

// Values is the narrow metadata adapter used by the existing details parser.
func Values(ctx context.Context, path, id string) map[string]interface{} {
	v := Read(ctx, path, id, Language(ctx))
	if v.Reason != "" {
		return nil
	}
	return map[string]interface{}{"name": v.Name, "author": v.Author, "version": v.Version, "description": v.Description}
}

type languageKey struct{}

func WithLanguage(ctx context.Context, language string) context.Context {
	if language != "en" {
		language = "zh"
	}
	return context.WithValue(ctx, languageKey{}, language)
}
func Language(ctx context.Context) string {
	if value, _ := ctx.Value(languageKey{}).(string); value == "en" {
		return "en"
	}
	return "zh"
}
