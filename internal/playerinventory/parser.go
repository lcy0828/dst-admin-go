// Package playerinventory reads serialized player data without executing Lua.
package playerinventory

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"

	"dont/shared"
)

const MaxInput = 2 << 20
const MaxDecoded = 8 << 20

type table map[any]any
type token struct {
	kind byte
	text string
}
type parser struct {
	ctx                        context.Context
	src                        []byte
	pos, tokens, tables, edges int
	tok                        token
	vars                       map[string]any
}

func failure(code string) error { return &inventoryError{code} }

type inventoryError struct{ code string }

func (e *inventoryError) Error() string { return e.code }

// Code keeps filesystem/parser details out of API responses.
func Code(err error) string {
	var remote *shared.InventoryError
	if errors.As(err, &remote) {
		return remote.Code
	}
	var e *inventoryError
	if errors.As(err, &e) {
		return e.code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "INVENTORY_TIMEOUT"
	}
	return "SAVE_INVALID"
}

func IsError(err error) bool {
	var local *inventoryError
	var remote *shared.InventoryError
	return errors.As(err, &local) || errors.As(err, &remote)
}

func decode(data []byte) ([]byte, error) {
	if len(data) > MaxInput {
		return nil, failure("SAVE_LIMIT_EXCEEDED")
	}
	if len(data) > 0 && data[0] == 3 {
		return decodeUserSession(data)
	}
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("KLEI0001")) {
		if len(data) < 11 {
			return nil, failure("SAVE_INVALID")
		}
		compact := bytes.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, bytes.TrimRight(data[11:], "\x00"))
		payload, err := base64.StdEncoding.DecodeString(string(compact))
		if err != nil || len(payload) <= 16 {
			return nil, failure("SAVE_INVALID")
		}
		r := flate.NewReader(bytes.NewReader(payload[16:]))
		defer r.Close()
		data, err = io.ReadAll(io.LimitReader(r, MaxDecoded+1))
		if err != nil {
			return nil, failure("SAVE_INVALID")
		}
		if len(data) > MaxDecoded {
			return nil, failure("SAVE_LIMIT_EXCEEDED")
		}
	} else if bytes.HasPrefix(data, []byte("KLEI")) {
		// Uncompressed PersistentStorage header: KLEI + whitespace + 1 + whitespace.
		rest := bytes.TrimLeft(data[4:], " \t\r\n\x00")
		if len(rest) == len(data)-4 || len(rest) < 2 || rest[0] != '1' || !strings.ContainsRune(" \t\r\n\x00", rune(rest[1])) {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		data = bytes.TrimLeft(rest[1:], " \t\r\n\x00")
	}
	if len(data) > 0 && data[0] == 3 {
		return decodeUserSession(data)
	}
	return bytes.TrimSpace(bytes.TrimRight(data, "\x00")), nil
}

// Native player snapshots use version 3 and a bit-packed string length. A
// leading bit selects 15 or 31 length bits; the last seven bits are aligned
// within the last byte. A final boolean belongs to the session envelope, not
// the Lua data. Validate the whole frame instead of searching for "return".
func decodeUserSession(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 3 {
		return nil, failure("SAVE_FORMAT_UNSUPPORTED")
	}
	width := 2
	if data[1]&0x80 != 0 {
		width = 4
	}
	if len(data) < width+2 {
		return nil, failure("SAVE_INVALID")
	}
	var length uint32
	for i := 0; i < width-1; i++ {
		value := uint32(data[1+i]&0x7f)<<1 | uint32(data[2+i]>>7)
		length |= value << (8 * i)
	}
	length |= uint32(data[width]&0x7f) << (8 * (width - 1))
	if length > MaxDecoded {
		return nil, failure("SAVE_LIMIT_EXCEEDED")
	}
	start := width + 1
	if uint64(start)+uint64(length)+1 != uint64(len(data)) || data[len(data)-1] > 1 {
		return nil, failure("SAVE_INVALID")
	}
	return data[start : start+int(length)], nil
}

func parse(ctx context.Context, data []byte) (*table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := decode(data)
	if err != nil {
		return nil, err
	}
	p := &parser{ctx: ctx, src: source, vars: map[string]any{}}
	if err = p.next(); err != nil {
		return nil, err
	}
	for p.tok.kind != 0 {
		if p.tok.text == "return" && p.tok.kind == 'i' {
			if err = p.next(); err != nil {
				return nil, err
			}
			v, e := p.value(0)
			if e != nil {
				return nil, e
			}
			if p.tok.kind == ';' {
				if e = p.next(); e != nil {
					return nil, e
				}
			}
			root, ok := v.(*table)
			if !ok || p.tok.kind != 0 {
				return nil, failure("SAVE_FORMAT_UNSUPPORTED")
			}
			return root, nil
		}
		local := p.tok.kind == 'i' && p.tok.text == "local"
		if local {
			if err = p.next(); err != nil {
				return nil, err
			}
		}
		if p.tok.kind != 'i' {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		name := p.tok.text
		if err = p.next(); err != nil {
			return nil, err
		}
		var destination *table
		var key any
		for p.tok.kind == '.' || p.tok.kind == '[' {
			if local {
				return nil, failure("SAVE_FORMAT_UNSUPPORTED")
			}
			var object any
			if destination == nil {
				object = p.vars[name]
			} else {
				object = (*destination)[key]
			}
			var ok bool
			destination, ok = object.(*table)
			if !ok {
				return nil, failure("SAVE_FORMAT_UNSUPPORTED")
			}
			key, err = p.attribute(0)
			if err != nil {
				return nil, err
			}
		}
		if p.tok.kind != '=' {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		if err = p.next(); err != nil {
			return nil, err
		}
		v, e := p.value(0)
		if e != nil {
			return nil, e
		}
		if destination != nil {
			(*destination)[key] = v
			p.edges++
		} else {
			if !local {
				if _, ok := p.vars[name]; !ok {
					return nil, failure("SAVE_FORMAT_UNSUPPORTED")
				}
			}
			p.vars[name] = v
		}
		if p.edges > 65536 {
			return nil, failure("SAVE_LIMIT_EXCEEDED")
		}
		if p.tok.kind == ';' {
			if err = p.next(); err != nil {
				return nil, err
			}
		}
	}
	return nil, failure("SAVE_FORMAT_UNSUPPORTED")
}

func (p *parser) attribute(depth int) (any, error) {
	if p.tok.kind == '.' {
		if err := p.next(); err != nil {
			return nil, err
		}
		if p.tok.kind != 'i' {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		k := p.tok.text
		return k, p.next()
	}
	if err := p.next(); err != nil {
		return nil, err
	}
	k, err := p.value(depth + 1)
	if err != nil {
		return nil, err
	}
	if !validKey(k) || p.tok.kind != ']' {
		return nil, failure("SAVE_FORMAT_UNSUPPORTED")
	}
	return k, p.next()
}
func validKey(v any) bool {
	switch v.(type) {
	case string, float64, bool:
		return true
	}
	return false
}

func (p *parser) value(depth int) (any, error) {
	if depth > 64 {
		return nil, failure("SAVE_LIMIT_EXCEEDED")
	}
	t := p.tok
	switch t.kind {
	case '{':
		p.tables++
		if p.tables > 16384 {
			return nil, failure("SAVE_LIMIT_EXCEEDED")
		}
		out := table{}
		if e := p.next(); e != nil {
			return nil, e
		}
		index := float64(1)
		for p.tok.kind != '}' {
			var key, value any
			var err error
			if p.tok.kind == '[' {
				if err = p.next(); err != nil {
					return nil, err
				}
				key, err = p.value(depth + 1)
				if err != nil {
					return nil, err
				}
				if !validKey(key) || p.tok.kind != ']' {
					return nil, failure("SAVE_FORMAT_UNSUPPORTED")
				}
				if err = p.next(); err != nil {
					return nil, err
				}
				if p.tok.kind != '=' {
					return nil, failure("SAVE_FORMAT_UNSUPPORTED")
				}
				if err = p.next(); err != nil {
					return nil, err
				}
				value, err = p.value(depth + 1)
			} else if p.tok.kind == 'i' && p.peekAssignment() {
				key = p.tok.text
				if err = p.next(); err != nil {
					return nil, err
				}
				if err = p.next(); err != nil {
					return nil, err
				}
				value, err = p.value(depth + 1)
			} else {
				key = index
				index++
				value, err = p.value(depth + 1)
			}
			if err != nil {
				return nil, err
			}
			p.edges++
			if p.edges > 65536 {
				return nil, failure("SAVE_LIMIT_EXCEEDED")
			}
			out[key] = value
			if p.tok.kind == ',' || p.tok.kind == ';' {
				if err = p.next(); err != nil {
					return nil, err
				}
			} else if p.tok.kind != '}' {
				return nil, failure("SAVE_FORMAT_UNSUPPORTED")
			}
		}
		return &out, p.next()
	case 's':
		return t.text, p.next()
	case 'n':
		v, e := strconv.ParseFloat(t.text, 64)
		if e != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, failure("SAVE_INVALID")
		}
		return v, p.next()
	case '-':
		if e := p.next(); e != nil {
			return nil, e
		}
		if p.tok.kind != 'n' {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		v, e := p.value(depth + 1)
		if e != nil {
			return nil, e
		}
		return -v.(float64), nil
	case 'i':
		if e := p.next(); e != nil {
			return nil, e
		}
		switch t.text {
		case "nil":
			return nil, nil
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		v, ok := p.vars[t.text]
		if !ok {
			return nil, failure("SAVE_FORMAT_UNSUPPORTED")
		}
		for p.tok.kind == '.' || p.tok.kind == '[' {
			tbl, ok := v.(*table)
			if !ok {
				return nil, failure("SAVE_FORMAT_UNSUPPORTED")
			}
			k, e := p.attribute(depth + 1)
			if e != nil {
				return nil, e
			}
			v = (*tbl)[k]
		}
		return v, nil
	}
	return nil, failure("SAVE_FORMAT_UNSUPPORTED")
}

func (p *parser) peekAssignment() bool {
	pos := p.pos
	for pos < len(p.src) && strings.ContainsRune(" \t\r\n", rune(p.src[pos])) {
		pos++
	}
	return pos < len(p.src) && p.src[pos] == '=' && (pos+1 == len(p.src) || p.src[pos+1] != '=')
}

func (p *parser) next() error {
	p.tokens++
	if p.tokens > 100000 {
		return failure("SAVE_LIMIT_EXCEEDED")
	}
	if p.tokens%256 == 0 {
		if err := p.ctx.Err(); err != nil {
			return err
		}
	}
	for p.pos < len(p.src) {
		if strings.ContainsRune(" \t\r\n\x00", rune(p.src[p.pos])) {
			p.pos++
			continue
		}
		if p.pos+1 < len(p.src) && string(p.src[p.pos:p.pos+2]) == "--" {
			p.pos += 2
			if p.pos < len(p.src) && p.src[p.pos] == '[' {
				if _, ok, err := p.longString(); err != nil {
					return err
				} else if ok {
					continue
				}
			}
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
	if p.pos == len(p.src) {
		p.tok = token{}
		return nil
	}
	start := p.pos
	c := p.src[p.pos]
	p.pos++
	if c == '\'' || c == '"' {
		var out strings.Builder
		for p.pos < len(p.src) {
			b := p.src[p.pos]
			p.pos++
			if b == c {
				p.tok = token{'s', out.String()}
				return nil
			}
			if b == '\\' {
				if p.pos == len(p.src) {
					return failure("SAVE_INVALID")
				}
				b = p.src[p.pos]
				p.pos++
				switch b {
				case 'a':
					b = 7
				case 'b':
					b = 8
				case 'f':
					b = 12
				case 'n':
					b = 10
				case 'r':
					b = 13
				case 't':
					b = 9
				case 'v':
					b = 11
				case '\\', '\'', '"', '\n':
				default:
					if b < '0' || b > '9' {
						return failure("SAVE_FORMAT_UNSUPPORTED")
					}
					v := int(b - '0')
					for i := 0; i < 2 && p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9'; i++ {
						v = v*10 + int(p.src[p.pos]-'0')
						p.pos++
					}
					if v > 255 {
						return failure("SAVE_INVALID")
					}
					b = byte(v)
				}
			}
			out.WriteByte(b)
			if out.Len() > 256<<10 {
				return failure("SAVE_LIMIT_EXCEEDED")
			}
		}
		return failure("SAVE_INVALID")
	}
	if c == '[' {
		p.pos = start
		if text, ok, e := p.longString(); e != nil {
			return e
		} else if ok {
			p.tok = token{'s', text}
			return nil
		}
		p.pos = start + 1
	}
	if c >= '0' && c <= '9' || c == '.' && p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		for p.pos < len(p.src) && strings.ContainsRune("0123456789.eE+-", rune(p.src[p.pos])) {
			p.pos++
		}
		p.tok = token{'n', string(p.src[start:p.pos])}
		return nil
	}
	if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
		for p.pos < len(p.src) {
			b := p.src[p.pos]
			if b != '_' && !(b >= 'A' && b <= 'Z') && !(b >= 'a' && b <= 'z') && !(b >= '0' && b <= '9') {
				break
			}
			p.pos++
		}
		if p.pos-start > 256<<10 {
			return failure("SAVE_LIMIT_EXCEEDED")
		}
		p.tok = token{'i', string(p.src[start:p.pos])}
		return nil
	}
	p.tok = token{c, string(c)}
	return nil
}

func (p *parser) longString() (string, bool, error) {
	start := p.pos
	end := start + 1
	for end < len(p.src) && p.src[end] == '=' {
		end++
	}
	if end >= len(p.src) || p.src[end] != '[' {
		return "", false, nil
	}
	close := "]" + strings.Repeat("=", end-start-1) + "]"
	from := end + 1
	index := bytes.Index(p.src[from:], []byte(close))
	if index < 0 {
		return "", false, failure("SAVE_INVALID")
	}
	if index > 256<<10 {
		return "", false, failure("SAVE_LIMIT_EXCEEDED")
	}
	text := string(p.src[from : from+index])
	if strings.HasPrefix(text, "\r\n") {
		text = text[2:]
	} else {
		text = strings.TrimPrefix(text, "\n")
	}
	p.pos = from + index + len(close)
	return text, true, nil
}
