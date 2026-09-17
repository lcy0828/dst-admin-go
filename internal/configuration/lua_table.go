package configuration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	luaParseLimit = 3 * time.Second
)

var luaIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

func parseLuaReturnTable(data []byte) (*luaNode, error) {
	ctx, cancel := context.WithTimeout(context.Background(), luaParseLimit)
	defer cancel()
	state := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 64, RegistrySize: 4096, RegistryMaxSize: 65536, MinimizeStackMemory: true})
	defer state.Close()
	state.SetContext(ctx)
	function, err := state.Load(bytes.NewReader(data), "leveldataoverride.lua")
	if err != nil {
		return nil, fmt.Errorf("compile leveldataoverride.lua: %w", err)
	}
	state.Push(function)
	if err := state.PCall(0, 1, nil); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("parse leveldataoverride.lua: timed out")
		}
		return nil, fmt.Errorf("evaluate leveldataoverride.lua: %w", err)
	}
	value := state.Get(-1)
	if value.Type() != lua.LTTable {
		return nil, errors.New("leveldataoverride.lua must return a table")
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
		if conversionErr == nil {
			sort.Slice(node.entries, func(i, j int) bool {
				return compareLuaTableKeys(node.entries[i].key, node.entries[j].key) < 0
			})
		}
		return node, conversionErr
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedLuaValue, value.Type().String())
	}
}

func compareLuaTableKeys(left, right *luaNode) int {
	leftRank := luaTableKeyRank(left.kind)
	rightRank := luaTableKeyRank(right.kind)
	if leftRank != rightRank {
		return leftRank - rightRank
	}
	switch left.kind {
	case lua.LTNumber:
		if left.number < right.number {
			return -1
		}
		if left.number > right.number {
			return 1
		}
	case lua.LTString:
		return strings.Compare(left.text, right.text)
	case lua.LTBool:
		if !left.boolean && right.boolean {
			return -1
		}
		if left.boolean && !right.boolean {
			return 1
		}
	}
	return 0
}

func luaTableKeyRank(kind lua.LValueType) int {
	switch kind {
	case lua.LTNumber:
		return 0
	case lua.LTString:
		return 1
	case lua.LTBool:
		return 2
	default:
		return 3
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
		if entry.key.kind == lua.LTString && entry.key.text == name {
			if value == nil {
				n.entries = append(n.entries[:index], n.entries[index+1:]...)
			} else {
				n.entries[index].value = value
			}
			return
		}
	}
	if value != nil {
		n.entries = append(n.entries, luaEntry{key: &luaNode{kind: lua.LTString, text: name}, value: value})
	}
}

func serializeLuaReturnTable(root *luaNode) ([]byte, error) {
	if root == nil || root.kind != lua.LTTable {
		return nil, errors.New("Lua root is not a table")
	}
	var output strings.Builder
	output.WriteString("return ")
	if err := writeLuaNode(&output, root, 0); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	return []byte(output.String()), nil
}

func writeLuaNode(output *strings.Builder, node *luaNode, depth int) error {
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
				if entry.key.kind == lua.LTString && luaIdentifier.MatchString(entry.key.text) {
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

func nodeToJSON(node *luaNode) (interface{}, error) {
	if node == nil {
		return nil, nil
	}
	switch node.kind {
	case lua.LTNil:
		return nil, nil
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
				value, err := nodeToJSON(item)
				if err != nil {
					return nil, err
				}
				result[index] = value
			}
			return result, nil
		}
		result := make(map[string]interface{}, len(node.entries))
		for _, entry := range node.entries {
			key, err := luaKeyString(entry.key)
			if err != nil {
				return nil, err
			}
			value, err := nodeToJSON(entry.value)
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

func luaKeyString(node *luaNode) (string, error) {
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

func jsonNode(raw json.RawMessage) (*luaNode, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	return jsonValueNode(value, 0)
}

func jsonValueNode(value interface{}, depth int) (*luaNode, error) {
	if depth > maxLuaDepth {
		return nil, ErrUnsupportedLuaValue
	}
	switch typed := value.(type) {
	case nil:
		return nil, ErrUnsupportedLuaValue
	case bool:
		return &luaNode{kind: lua.LTBool, boolean: typed}, nil
	case string:
		return &luaNode{kind: lua.LTString, text: typed}, nil
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return nil, err
		}
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, ErrUnsupportedLuaValue
		}
		return &luaNode{kind: lua.LTNumber, number: number}, nil
	case []interface{}:
		node := &luaNode{kind: lua.LTTable, entries: make([]luaEntry, 0, len(typed))}
		for index, item := range typed {
			valueNode, err := jsonValueNode(item, depth+1)
			if err != nil {
				return nil, err
			}
			node.entries = append(node.entries, luaEntry{key: &luaNode{kind: lua.LTNumber, number: float64(index + 1)}, value: valueNode})
		}
		return node, nil
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		node := &luaNode{kind: lua.LTTable, entries: make([]luaEntry, 0, len(keys))}
		for _, key := range keys {
			valueNode, err := jsonValueNode(typed[key], depth+1)
			if err != nil {
				return nil, err
			}
			if valueNode == nil {
				continue
			}
			node.entries = append(node.entries, luaEntry{key: &luaNode{kind: lua.LTString, text: key}, value: valueNode})
		}
		return node, nil
	default:
		return nil, ErrUnsupportedLuaValue
	}
}
