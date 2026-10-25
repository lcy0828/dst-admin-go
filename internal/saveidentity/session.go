// Package saveidentity reads DST's save selection without evaluating save Lua.
package saveidentity

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

const maxIndexBytes = 4 * 1024 * 1024

var ErrInvalidIndex = errors.New("DST save index is invalid")

// The index changes at saves, not at the telemetry interval. Cache only parsed
// identities, bounded by file identity and metadata; never cache directory guesses.
var indexCache = struct {
	sync.Mutex
	entries map[string]indexEntry
}{entries: make(map[string]indexEntry)}

type indexEntry struct {
	info    os.FileInfo
	session string
	err     error
}

// IndexedSession returns the session selected by shardindex or legacy slot 1.
// exists distinguishes an absent index from an empty/new world index.
func IndexedSession(worldRoot string) (session string, exists bool, err error) {
	if err := directory(filepath.Join(worldRoot, "save")); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	session, exists, err = readIndex(filepath.Join(worldRoot, "save", "shardindex"), false)
	if exists && err == nil {
		return session, true, nil
	}
	legacy, legacyExists, legacyErr := readIndex(filepath.Join(worldRoot, "save", "saveindex"), true)
	if legacyExists && legacyErr == nil {
		return legacy, true, nil
	}
	if exists || err != nil {
		return "", exists, err
	}
	return legacy, legacyExists, legacyErr
}

// CurrentSession uses the game's index. Old installations without an index
// remain readable only when a single directory makes the choice unambiguous.
func CurrentSession(worldRoot string) (string, error) {
	session, exists, err := IndexedSession(worldRoot)
	if err != nil || exists {
		return session, err
	}
	root := filepath.Join(worldRoot, "save", "session")
	if err := directory(root); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validSession(entry.Name()) {
			continue
		}
		if session != "" {
			return "", fmt.Errorf("%w: multiple sessions without a save index", ErrInvalidIndex)
		}
		session = entry.Name()
	}
	return session, nil
}

func directory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: unsafe save directory", ErrInvalidIndex)
	}
	return nil
}

func readIndex(path string, legacy bool) (string, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxIndexBytes {
		return "", true, ErrInvalidIndex
	}
	indexCache.Lock()
	cached, found := indexCache.entries[path]
	indexCache.Unlock()
	if found && sameFile(cached.info, info) {
		return cached.session, true, cached.err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", true, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFile(info, opened) {
		return "", true, ErrInvalidIndex
	}
	data, err := io.ReadAll(io.LimitReader(file, maxIndexBytes+1))
	if err != nil {
		return "", true, err
	}
	after, err := file.Stat()
	if err != nil || len(data) > maxIndexBytes || int64(len(data)) != info.Size() || !sameFile(opened, after) {
		return "", true, ErrInvalidIndex
	}
	session, parseErr := sessionFromIndex(data, legacy)
	indexCache.Lock()
	if len(indexCache.entries) >= 256 { // No worker or idle polling is needed.
		for key := range indexCache.entries {
			delete(indexCache.entries, key)
			break
		}
	}
	indexCache.entries[path] = indexEntry{info: after, session: session, err: parseErr}
	indexCache.Unlock()
	return session, true, parseErr
}

func sameFile(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime()) && left.Mode() == right.Mode()
}

func sessionFromIndex(data []byte, legacy bool) (string, error) {
	data = bytes.TrimSpace(bytes.TrimRight(data, "\x00"))
	if bytes.HasPrefix(data, []byte("KLEI")) {
		// Index files are written with SetPersistentString(..., false).
		rest := bytes.TrimLeft(data[4:], " \t\r\n\x00")
		if len(rest) == len(data)-4 || len(rest) == 0 || rest[0] != '1' {
			return "", ErrInvalidIndex
		}
		rest = rest[1:]
		if len(rest) == 0 || !strings.ContainsRune(" \t\r\n\x00", rune(rest[0])) {
			return "", ErrInvalidIndex
		}
		data = bytes.TrimLeft(rest, " \t\r\n\x00")
	}
	statements, err := parse.Parse(bytes.NewReader(data), "save index")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	// Accept only serialized data, never calls or control flow. Resolve the
	// assignments emitted by DataDumper for shared tables as well as literals.
	locals := make(map[string]ast.Expr)
	var root *ast.TableExpr
	for index, stmt := range statements {
		switch value := stmt.(type) {
		case *ast.LocalAssignStmt:
			if len(value.Names) != len(value.Exprs) {
				return "", ErrInvalidIndex
			}
			evaluated := make([]ast.Expr, len(value.Exprs))
			for i, expr := range value.Exprs {
				evaluated[i], err = serializedValue(expr, locals, 0)
				if err != nil {
					return "", err
				}
			}
			for i, name := range value.Names {
				locals[name] = evaluated[i]
			}
		case *ast.AssignStmt:
			if len(value.Lhs) != 1 || len(value.Rhs) != 1 {
				return "", ErrInvalidIndex
			}
			rhs, err := serializedValue(value.Rhs[0], locals, 0)
			if err != nil {
				return "", err
			}
			switch lhs := value.Lhs[0].(type) {
			case *ast.IdentExpr:
				if _, ok := locals[lhs.Value]; !ok {
					return "", ErrInvalidIndex
				}
				locals[lhs.Value] = rhs
			case *ast.AttrGetExpr:
				object, err := serializedValue(lhs.Object, locals, 0)
				table, ok := object.(*ast.TableExpr)
				if err != nil || !ok || fieldKey(lhs.Key) == "" {
					return "", ErrInvalidIndex
				}
				table.Fields = append(table.Fields, &ast.Field{Key: lhs.Key, Value: rhs})
			default:
				return "", ErrInvalidIndex
			}
		case *ast.ReturnStmt:
			if index != len(statements)-1 || len(value.Exprs) != 1 {
				return "", ErrInvalidIndex
			}
			evaluated, err := serializedValue(value.Exprs[0], locals, 0)
			if err != nil {
				return "", err
			}
			root, _ = evaluated.(*ast.TableExpr)
		default:
			return "", ErrInvalidIndex
		}
	}
	if root == nil {
		return "", ErrInvalidIndex
	}
	if legacy {
		slots, ok := tableField(root, "slots").(*ast.TableExpr)
		if !ok {
			return "", ErrInvalidIndex
		}
		root, ok = fieldAt(slots, "n:1").(*ast.TableExpr)
		if !ok {
			return "", ErrInvalidIndex
		}
	}
	value := tableField(root, "session_id")
	if value == nil {
		return "", nil
	}
	if _, ok := value.(*ast.NilExpr); ok {
		return "", nil
	}
	text, ok := value.(*ast.StringExpr)
	if !ok || !validSession(text.Value) {
		return "", ErrInvalidIndex
	}
	return text.Value, nil
}

func tableField(table *ast.TableExpr, name string) ast.Expr { return fieldAt(table, "s:"+name) }

func fieldKey(key ast.Expr) string {
	switch v := key.(type) {
	case *ast.StringExpr:
		return "s:" + v.Value
	case *ast.UnaryMinusOpExpr:
		if number, ok := v.Expr.(*ast.NumberExpr); ok {
			if n, err := strconv.ParseFloat(number.Value, 64); err == nil {
				return "n:" + strconv.FormatFloat(-n, 'g', -1, 64)
			}
		}
	case *ast.NumberExpr:
		if n, err := strconv.ParseFloat(v.Value, 64); err == nil {
			return "n:" + strconv.FormatFloat(n, 'g', -1, 64)
		}
	}
	return ""
}

func fieldAt(table *ast.TableExpr, key string) ast.Expr {
	var value ast.Expr
	next := 1
	for _, field := range table.Fields {
		actual := fieldKey(field.Key)
		if field.Key == nil {
			actual = "n:" + strconv.Itoa(next)
			next++
		}
		if actual == key {
			value = field.Value
		}
	}
	return value
}

// Construct table data once, using references to already constructed tables.
// Scalar aliases are captured at assignment time, exactly as in Lua, while
// DataDumper's later table-reference assignments retain shared identity.
func serializedValue(expr ast.Expr, locals map[string]ast.Expr, depth int) (ast.Expr, error) {
	if depth > 128 {
		return nil, ErrInvalidIndex
	}
	switch v := expr.(type) {
	case *ast.StringExpr, *ast.NumberExpr, *ast.NilExpr, *ast.TrueExpr, *ast.FalseExpr:
		return expr, nil
	case *ast.IdentExpr:
		if value, ok := locals[v.Value]; ok {
			return value, nil
		}
		return &ast.NilExpr{}, nil
	case *ast.UnaryMinusOpExpr:
		if _, ok := v.Expr.(*ast.NumberExpr); ok {
			return expr, nil
		}
	case *ast.AttrGetExpr:
		object, err := serializedValue(v.Object, locals, depth+1)
		table, ok := object.(*ast.TableExpr)
		if err != nil || !ok || fieldKey(v.Key) == "" {
			return nil, ErrInvalidIndex
		}
		if value := fieldAt(table, fieldKey(v.Key)); value != nil {
			return value, nil
		}
		return &ast.NilExpr{}, nil
	case *ast.TableExpr:
		table := &ast.TableExpr{}
		for _, field := range v.Fields {
			if field.Key != nil && fieldKey(field.Key) == "" {
				return nil, ErrInvalidIndex
			}
			value, err := serializedValue(field.Value, locals, depth+1)
			if err != nil {
				return nil, err
			}
			table.Fields = append(table.Fields, &ast.Field{Key: field.Key, Value: value})
		}
		return table, nil
	}
	return nil, ErrInvalidIndex
}

func validSession(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 128 && !strings.HasPrefix(value, ".") && !strings.ContainsAny(value, "/\\\x00\r\n")
}
