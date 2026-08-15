package mods

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const (
	maxLuaDepth   = 100
	maxLuaEntries = 100000
	maxLuaBytes   = int64(4 * 1024 * 1024)
)

var luaIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type luaNode struct {
	kind    lua.LValueType
	boolean bool
	number  float64
	text    string
	entries []luaEntry
}

type luaEntry struct {
	key   *luaNode
	value *luaNode
}

type modOverrideDocument struct {
	root     *luaNode
	data     []byte
	mode     os.FileMode
	exists   bool
	revision string
}

func loadModOverride(path string) (modOverrideDocument, error) {
	data, mode, exists, err := readModFile(path, true)
	if err != nil {
		return modOverrideDocument{}, err
	}
	if !exists {
		data = []byte("return {}\n")
		mode = 0640
	}
	return parseModOverrideContent(data, mode, exists, filepath.Base(path))
}

func parseModOverrideContent(data []byte, mode os.FileMode, exists bool, name string) (modOverrideDocument, error) {
	if len(data) == 0 {
		data = []byte("return {}\n")
	}
	root, err := parseLuaTable(data, name)
	if err != nil {
		return modOverrideDocument{}, err
	}
	return modOverrideDocument{root: root, data: append([]byte(nil), data...), mode: mode, exists: exists, revision: contentRevision(data)}, nil
}

func parseLuaTable(data []byte, name string) (*luaNode, error) {
	if int64(len(data)) > maxLuaBytes {
		return nil, errors.New("Lua file exceeds 4 MiB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 64, RegistrySize: 4096, RegistryMaxSize: 65536, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(ctx)
	function, err := state.Load(bytes.NewReader(data), name)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", name, err)
	}
	state.Push(function)
	if err := state.PCall(0, 1, nil); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("evaluate %s: timed out", name)
		}
		return nil, fmt.Errorf("evaluate %s: %w", name, err)
	}
	value := state.Get(-1)
	if value.Type() != lua.LTTable {
		return nil, fmt.Errorf("%s must return a table", name)
	}
	count := 0
	return luaValueNode(value, make(map[*lua.LTable]bool), 0, &count)
}

func luaValueNode(value lua.LValue, visiting map[*lua.LTable]bool, depth int, count *int) (*luaNode, error) {
	if depth > maxLuaDepth {
		return nil, fmt.Errorf("%w: nesting exceeds %d", ErrUnsupportedLuaValue, maxLuaDepth)
	}
	switch value.Type() {
	case lua.LTNil:
		return &luaNode{kind: lua.LTNil}, nil
	case lua.LTBool:
		return &luaNode{kind: lua.LTBool, boolean: bool(value.(lua.LBool))}, nil
	case lua.LTNumber:
		number := float64(value.(lua.LNumber))
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("%w: non-finite number", ErrUnsupportedLuaValue)
		}
		return &luaNode{kind: lua.LTNumber, number: number}, nil
	case lua.LTString:
		return &luaNode{kind: lua.LTString, text: string(value.(lua.LString))}, nil
	case lua.LTTable:
		table := value.(*lua.LTable)
		if visiting[table] {
			return nil, fmt.Errorf("%w: cyclic table", ErrUnsupportedLuaValue)
		}
		visiting[table] = true
		defer delete(visiting, table)
		node := &luaNode{kind: lua.LTTable}
		var conversionErr error
		table.ForEach(func(key, item lua.LValue) {
			if conversionErr != nil {
				return
			}
			*count++
			if *count > maxLuaEntries {
				conversionErr = fmt.Errorf("%w: table exceeds %d entries", ErrUnsupportedLuaValue, maxLuaEntries)
				return
			}
			keyNode, err := luaValueNode(key, visiting, depth+1, count)
			if err != nil {
				conversionErr = err
				return
			}
			valueNode, err := luaValueNode(item, visiting, depth+1, count)
			if err != nil {
				conversionErr = err
				return
			}
			if keyNode.kind != lua.LTString && keyNode.kind != lua.LTNumber && keyNode.kind != lua.LTBool {
				conversionErr = fmt.Errorf("%w: table key type %s", ErrUnsupportedLuaValue, keyNode.kind.String())
				return
			}
			node.entries = append(node.entries, luaEntry{key: keyNode, value: valueNode})
		})
		sort.Slice(node.entries, func(left, right int) bool {
			return luaNodeKeyLess(node.entries[left].key, node.entries[right].key)
		})
		return node, conversionErr
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedLuaValue, value.Type().String())
	}
}

func luaNodeKeyLess(left, right *luaNode) bool {
	if left.kind != right.kind {
		return left.kind < right.kind
	}
	switch left.kind {
	case lua.LTNumber:
		return left.number < right.number
	case lua.LTString:
		return left.text < right.text
	case lua.LTBool:
		return !left.boolean && right.boolean
	default:
		return false
	}
}

func (n *luaNode) stringEntry(name string) (*luaNode, bool) {
	if n == nil || n.kind != lua.LTTable {
		return nil, false
	}
	for _, entry := range n.entries {
		if entry.key.kind == lua.LTString && entry.key.text == name {
			return entry.value, true
		}
	}
	return nil, false
}

func (n *luaNode) setStringEntry(name string, value *luaNode) {
	for index, entry := range n.entries {
		if entry.key.kind != lua.LTString || entry.key.text != name {
			continue
		}
		if value == nil {
			n.entries = append(n.entries[:index], n.entries[index+1:]...)
		} else {
			n.entries[index].value = value
		}
		return
	}
	if value != nil {
		n.entries = append(n.entries, luaEntry{key: stringNode(name), value: value})
	}
}

func (d modOverrideDocument) mod(modID string) (*luaNode, bool) {
	return d.root.stringEntry("workshop-" + modID)
}

func modEnabled(entry *luaNode) bool {
	value, ok := entry.stringEntry("enabled")
	return ok && value.kind == lua.LTBool && value.boolean
}

func modConfiguration(entry *luaNode) (*luaNode, bool) {
	value, ok := entry.stringEntry("configuration_options")
	return value, ok && value.kind == lua.LTTable
}

func ensureModEntry(document *modOverrideDocument, modID string, enabled bool) *luaNode {
	entry, ok := document.mod(modID)
	if !ok || entry.kind != lua.LTTable {
		entry = tableNode()
		document.root.setStringEntry("workshop-"+modID, entry)
	}
	entry.setStringEntry("enabled", boolNode(enabled))
	if _, ok := modConfiguration(entry); !ok {
		entry.setStringEntry("configuration_options", tableNode())
	}
	return entry
}

func renderModOverride(document modOverrideDocument) ([]byte, error) {
	var output strings.Builder
	output.WriteString("return ")
	if err := writeLuaNode(&output, document.root, 0); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	return []byte(output.String()), nil
}

func writeLuaNode(output *strings.Builder, node *luaNode, depth int) error {
	if node == nil {
		output.WriteString("nil")
		return nil
	}
	switch node.kind {
	case lua.LTNil:
		output.WriteString("nil")
	case lua.LTBool:
		output.WriteString(strconv.FormatBool(node.boolean))
	case lua.LTNumber:
		output.WriteString(strconv.FormatFloat(node.number, 'g', -1, 64))
	case lua.LTString:
		output.WriteString(strconv.Quote(node.text))
	case lua.LTTable:
		output.WriteByte('{')
		if len(node.entries) > 0 {
			output.WriteByte('\n')
			for _, entry := range node.entries {
				output.WriteString(strings.Repeat("  ", depth+1))
				if entry.key.kind == lua.LTString && luaIdentifierPattern.MatchString(entry.key.text) {
					output.WriteString(entry.key.text)
				} else {
					output.WriteByte('[')
					if err := writeLuaNode(output, entry.key, depth+1); err != nil {
						return err
					}
					output.WriteByte(']')
				}
				output.WriteString(" = ")
				if err := writeLuaNode(output, entry.value, depth+1); err != nil {
					return err
				}
				output.WriteString(",\n")
			}
			output.WriteString(strings.Repeat("  ", depth))
		}
		output.WriteByte('}')
	default:
		return ErrUnsupportedLuaValue
	}
	return nil
}

func nodeToInterface(node *luaNode) (interface{}, error) {
	if node == nil || node.kind == lua.LTNil {
		return nil, nil
	}
	switch node.kind {
	case lua.LTBool:
		return node.boolean, nil
	case lua.LTNumber:
		return node.number, nil
	case lua.LTString:
		return node.text, nil
	case lua.LTTable:
		if array, ok := nodeArray(node); ok {
			result := make([]interface{}, len(array))
			for index, item := range array {
				value, err := nodeToInterface(item)
				if err != nil {
					return nil, err
				}
				result[index] = value
			}
			return result, nil
		}
		result := make(map[string]interface{}, len(node.entries))
		for _, entry := range node.entries {
			key, err := nodeKeyString(entry.key)
			if err != nil {
				return nil, err
			}
			value, err := nodeToInterface(entry.value)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		return result, nil
	default:
		return nil, ErrUnsupportedLuaValue
	}
}

func nodeArray(node *luaNode) ([]*luaNode, bool) {
	if len(node.entries) == 0 {
		return []*luaNode{}, true
	}
	result := make([]*luaNode, len(node.entries))
	for _, entry := range node.entries {
		if entry.key.kind != lua.LTNumber || entry.key.number < 1 || entry.key.number != float64(int(entry.key.number)) || int(entry.key.number) > len(result) {
			return nil, false
		}
		index := int(entry.key.number) - 1
		if result[index] != nil {
			return nil, false
		}
		result[index] = entry.value
	}
	return result, true
}

func nodeKeyString(node *luaNode) (string, error) {
	switch node.kind {
	case lua.LTString:
		return node.text, nil
	case lua.LTNumber:
		return strconv.FormatFloat(node.number, 'g', -1, 64), nil
	case lua.LTBool:
		return strconv.FormatBool(node.boolean), nil
	default:
		return "", ErrUnsupportedLuaValue
	}
}

func rawJSONNode(raw json.RawMessage) (*luaNode, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	if value == nil {
		return nil, true, nil
	}
	node, err := jsonValueNode(value, 0)
	return node, false, err
}

func jsonValueNode(value interface{}, depth int) (*luaNode, error) {
	if depth > maxLuaDepth {
		return nil, ErrUnsupportedLuaValue
	}
	switch typed := value.(type) {
	case nil:
		return nil, ErrUnsupportedLuaValue
	case bool:
		return boolNode(typed), nil
	case string:
		return stringNode(typed), nil
	case json.Number:
		number, err := typed.Float64()
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, ErrUnsupportedLuaValue
		}
		return &luaNode{kind: lua.LTNumber, number: number}, nil
	case []interface{}:
		node := tableNode()
		for index, item := range typed {
			itemNode, err := jsonValueNode(item, depth+1)
			if err != nil {
				return nil, err
			}
			node.entries = append(node.entries, luaEntry{key: &luaNode{kind: lua.LTNumber, number: float64(index + 1)}, value: itemNode})
		}
		return node, nil
	case map[string]interface{}:
		node := tableNode()
		for key, item := range typed {
			itemNode, err := jsonValueNode(item, depth+1)
			if err != nil {
				return nil, err
			}
			node.setStringEntry(key, itemNode)
		}
		return node, nil
	default:
		return nil, ErrUnsupportedLuaValue
	}
}

func tableNode() *luaNode              { return &luaNode{kind: lua.LTTable} }
func stringNode(value string) *luaNode { return &luaNode{kind: lua.LTString, text: value} }
func boolNode(value bool) *luaNode     { return &luaNode{kind: lua.LTBool, boolean: value} }

func readModFile(path string, optional bool) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && optional {
		return nil, 0640, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxLuaBytes {
		return nil, 0, false, errors.New("mod configuration file is unsafe or too large")
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), true, err
}

func atomicWriteModFile(path string, data []byte, mode os.FileMode) error {
	if int64(len(data)) > maxLuaBytes {
		return errors.New("mod configuration file exceeds 4 MiB")
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".dst-admin-mod-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	published := false
	defer func() {
		_ = file.Close()
		if !published {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	published = true
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

func contentRevision(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
