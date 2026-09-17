package steamvdf

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type tokenKind uint8

const (
	stringToken tokenKind = iota
	openToken
	closeToken
)

type token struct {
	kind tokenKind
	text string
}

func Parse(data []byte) (map[string]interface{}, error) {
	tokens, err := tokenize(data)
	if err != nil {
		return nil, err
	}
	position := 0
	result, err := parseObject(tokens, &position, false, 0)
	if err != nil {
		return nil, err
	}
	if position != len(tokens) {
		return nil, errors.New("unexpected trailing tokens")
	}
	return result, nil
}

func Marshal(value map[string]interface{}) ([]byte, error) {
	var builder strings.Builder
	if err := writeObject(&builder, value, 0, false); err != nil {
		return nil, err
	}
	return []byte(builder.String()), nil
}

func writeObject(builder *strings.Builder, value map[string]interface{}, depth int, nested bool) error {
	if depth > 64 {
		return errors.New("Valve KeyValues nesting is too deep")
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	indent := strings.Repeat("\t", depth)
	for _, key := range keys {
		if key == "" {
			return errors.New("Valve KeyValues key is empty")
		}
		builder.WriteString(indent)
		writeQuoted(builder, key)
		switch item := value[key].(type) {
		case string:
			builder.WriteString("\t\t")
			writeQuoted(builder, item)
			builder.WriteByte('\n')
		case map[string]interface{}:
			builder.WriteByte('\n')
			builder.WriteString(indent)
			builder.WriteString("{\n")
			if err := writeObject(builder, item, depth+1, true); err != nil {
				return err
			}
			builder.WriteString(indent)
			builder.WriteString("}\n")
		default:
			return fmt.Errorf("unsupported Valve KeyValues value for %q", key)
		}
	}
	if !nested && depth != 0 {
		return errors.New("invalid Valve KeyValues root")
	}
	return nil
}

func writeQuoted(builder *strings.Builder, value string) {
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\', '"':
			builder.WriteByte('\\')
			builder.WriteRune(character)
		case '\n':
			builder.WriteString("\\n")
		case '\t':
			builder.WriteString("\\t")
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
}

func parseObject(tokens []token, position *int, nested bool, depth int) (map[string]interface{}, error) {
	if depth > 64 {
		return nil, errors.New("Valve KeyValues nesting is too deep")
	}
	result := make(map[string]interface{})
	for *position < len(tokens) {
		if tokens[*position].kind == closeToken {
			if !nested {
				return nil, errors.New("unexpected closing brace")
			}
			(*position)++
			return result, nil
		}
		key := tokens[*position]
		(*position)++
		if key.kind != stringToken || *position >= len(tokens) {
			return nil, errors.New("expected a KeyValues key and value")
		}
		next := tokens[*position]
		(*position)++
		switch next.kind {
		case stringToken:
			result[key.text] = next.text
		case openToken:
			child, err := parseObject(tokens, position, true, depth+1)
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

func tokenize(data []byte) ([]token, error) {
	tokens := make([]token, 0, 256)
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
			tokens = append(tokens, token{kind: openToken})
			index++
		case '}':
			tokens = append(tokens, token{kind: closeToken})
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
			tokens = append(tokens, token{kind: stringToken, text: value.String()})
		default:
			return nil, fmt.Errorf("unexpected KeyValues byte 0x%02x", data[index])
		}
		if len(tokens) > 200000 {
			return nil, errors.New("Valve KeyValues contains too many tokens")
		}
	}
	return tokens, nil
}
