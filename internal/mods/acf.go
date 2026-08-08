package mods

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxACFBytes = int64(8 * 1024 * 1024)

type workshopManifestItem struct {
	Manifest  string
	UpdatedAt time.Time
}

func loadWorkshopManifest(path string) (map[string]workshopManifestItem, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return map[string]workshopManifestItem{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxACFBytes {
		return nil, errors.New("Steam workshop manifest is unsafe or too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := parseValveKeyValues(data)
	if err != nil {
		return nil, fmt.Errorf("parse Steam workshop manifest: %w", err)
	}
	app, _ := root["AppWorkshop"].(map[string]interface{})
	installed, _ := app["WorkshopItemsInstalled"].(map[string]interface{})
	result := make(map[string]workshopManifestItem, len(installed))
	for id, raw := range installed {
		if !validModID(id) {
			continue
		}
		fields, _ := raw.(map[string]interface{})
		item := workshopManifestItem{}
		item.Manifest, _ = fields["manifest"].(string)
		stamp, _ := fields["timeupdated"].(string)
		seconds, parseErr := strconv.ParseInt(stamp, 10, 64)
		if parseErr == nil && seconds > 0 {
			item.UpdatedAt = time.Unix(seconds, 0).UTC()
		}
		result[id] = item
	}
	return result, nil
}

type valveTokenKind uint8

const (
	valveString valveTokenKind = iota
	valveOpen
	valveClose
)

type valveToken struct {
	kind valveTokenKind
	text string
}

func parseValveKeyValues(data []byte) (map[string]interface{}, error) {
	tokens, err := tokenizeValveKeyValues(data)
	if err != nil {
		return nil, err
	}
	position := 0
	result, err := parseValveObject(tokens, &position, false, 0)
	if err != nil {
		return nil, err
	}
	if position != len(tokens) {
		return nil, errors.New("unexpected trailing tokens")
	}
	return result, nil
}

func parseValveObject(tokens []valveToken, position *int, nested bool, depth int) (map[string]interface{}, error) {
	if depth > 64 {
		return nil, errors.New("Valve KeyValues nesting is too deep")
	}
	result := make(map[string]interface{})
	for *position < len(tokens) {
		if tokens[*position].kind == valveClose {
			if !nested {
				return nil, errors.New("unexpected closing brace")
			}
			(*position)++
			return result, nil
		}
		key := tokens[*position]
		(*position)++
		if key.kind != valveString || *position >= len(tokens) {
			return nil, errors.New("expected a KeyValues key and value")
		}
		next := tokens[*position]
		(*position)++
		switch next.kind {
		case valveString:
			result[key.text] = next.text
		case valveOpen:
			child, err := parseValveObject(tokens, position, true, depth+1)
			if err != nil {
				return nil, err
			}
			result[key.text] = child
		default:
			return nil, errors.New("expected a KeyValues value or opening brace")
		}
	}
	if nested {
		return nil, errors.New("unterminated KeyValues object")
	}
	return result, nil
}

func tokenizeValveKeyValues(data []byte) ([]valveToken, error) {
	tokens := make([]valveToken, 0, 256)
	for index := 0; index < len(data); {
		switch data[index] {
		case ' ', '\t', '\r', '\n':
			index++
		case '/':
			if index+1 >= len(data) || data[index+1] != '/' {
				return nil, errors.New("unexpected slash")
			}
			index += 2
			for index < len(data) && data[index] != '\n' {
				index++
			}
		case '{':
			tokens = append(tokens, valveToken{kind: valveOpen})
			index++
		case '}':
			tokens = append(tokens, valveToken{kind: valveClose})
			index++
		case '"':
			index++
			var value strings.Builder
			closed := false
			for index < len(data) {
				character := data[index]
				index++
				if character == '"' {
					closed = true
					break
				}
				if character == '\\' {
					if index >= len(data) {
						return nil, errors.New("unterminated KeyValues escape")
					}
					escaped := data[index]
					index++
					switch escaped {
					case '\\', '"':
						value.WriteByte(escaped)
					case 'n':
						value.WriteByte('\n')
					case 't':
						value.WriteByte('\t')
					default:
						value.WriteByte(escaped)
					}
					continue
				}
				value.WriteByte(character)
			}
			if !closed {
				return nil, errors.New("unterminated KeyValues string")
			}
			tokens = append(tokens, valveToken{kind: valveString, text: value.String()})
		default:
			return nil, fmt.Errorf("unexpected KeyValues byte 0x%02x", data[index])
		}
		if len(tokens) > 200000 {
			return nil, errors.New("Valve KeyValues contains too many tokens")
		}
	}
	return tokens, nil
}
