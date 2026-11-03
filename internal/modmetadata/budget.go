package modmetadata

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

const maxValueBytes = 64 << 10

var errInstructionBudget = errors.New("metadata instruction budget exceeded")

// The VM checks Done before each instruction. Bound instruction count as well
// as wall time so a fast host cannot allocate an unbounded number of tables.
type evaluationBudget struct {
	context.Context
	remaining int
	exhausted chan struct{}
}

func (b *evaluationBudget) Done() <-chan struct{} {
	if b.remaining > 0 {
		b.remaining--
		return b.Context.Done()
	}
	return b.exhausted
}
func (b *evaluationBudget) Err() error {
	if b.remaining == 0 {
		return errInstructionBudget
	}
	return b.Context.Err()
}
func withInstructionBudget(ctx context.Context, sourceBytes int) context.Context {
	exhausted := make(chan struct{})
	close(exhausted)
	// Large translated option tables need more VM work than a small identity
	// file. This remains a fixed ceiling in addition to the wall/allocation caps.
	return &evaluationBudget{Context: ctx, remaining: min(100000, max(20000, sourceBytes/2)), exhausted: exhausted}
}

// Replacing concatenation before compilation catches both local and global
// strings, including concatenation in dependencies and nested functions.
func boundedLoad(L *lua.LState, data []byte, path string) (*lua.LFunction, error) {
	chunk, err := parse.Parse(bytes.NewReader(data), path)
	if err != nil {
		return nil, err
	}
	return boundedCompile(L, chunk, path)
}

func boundedCompile(L *lua.LState, chunk []ast.Stmt, path string) (*lua.LFunction, error) {
	var invalid bool
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Interface && !v.IsNil() && v.CanSet() {
			if ident, ok := v.Interface().(*ast.IdentExpr); ok && strings.HasPrefix(ident.Value, "__dst_metadata_") {
				invalid = true
			}
			if access, ok := v.Interface().(*ast.AttrGetExpr); ok {
				// Visit the original key before injecting a reserved helper.
				key := access.Key
				walk(reflect.ValueOf(&key).Elem())
				access.Key = boundedKey(key)
				walk(reflect.ValueOf(&access.Object).Elem())
				return
			}
			if table, ok := v.Interface().(*ast.TableExpr); ok {
				for _, field := range table.Fields {
					walk(reflect.ValueOf(&field.Value).Elem())
					if field.Key != nil {
						walk(reflect.ValueOf(&field.Key).Elem())
						field.Key = boundedKey(field.Key)
					}
				}
				return
			}
			if concat, ok := v.Interface().(*ast.StringConcatOpExpr); ok {
				walk(reflect.ValueOf(&concat.Lhs).Elem())
				walk(reflect.ValueOf(&concat.Rhs).Elem())
				call := &ast.FuncCallExpr{Func: &ast.IdentExpr{Value: "__dst_metadata_concat"}, Args: []ast.Expr{concat.Lhs, concat.Rhs}}
				v.Set(reflect.ValueOf(call))
				return
			}
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).CanInterface() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		}
	}
	walk(reflect.ValueOf(chunk))
	if invalid {
		return nil, errors.New("reserved metadata identifier")
	}
	proto, err := lua.Compile(chunk, path)
	if err != nil {
		return nil, err
	}
	return L.NewFunctionFromProto(proto), nil
}

// Literal string keys cannot cause a sparse numeric allocation. Instrumenting
// every named field multiplies the VM work of ordinary large mod configurations.
// Dynamic and numeric keys still use the existing allocation guard.
func boundedKey(key ast.Expr) ast.Expr {
	if _, ok := key.(*ast.StringExpr); ok {
		return key
	}
	return &ast.FuncCallExpr{Func: &ast.IdentExpr{Value: "__dst_metadata_key"}, Args: []ast.Expr{key}}
}

// Bound the expansion before invoking the native formatter. A fixed cap on
// percent signs rejected ordinary descriptions with nine short placeholders.
func formatFitsBudget(L *lua.LState) bool {
	format := L.CheckString(1)
	if len(format) > 1024 {
		return false
	}
	bound, argument := len(format), 2
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i >= len(format) {
			return false
		}
		if format[i] == '%' {
			continue
		}
		for i < len(format) && strings.ContainsRune("-+ #0", rune(format[i])) {
			i++
		}
		readNumber := func() (int, bool) {
			start := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			if start == i {
				return 0, true
			}
			if i-start > 5 {
				return 0, false
			}
			value, err := strconv.Atoi(format[start:i])
			return value, err == nil && value <= maxValueBytes
		}
		width, ok := readNumber()
		if !ok {
			return false
		}
		precision := 0
		if i < len(format) && format[i] == '.' {
			i++
			precision, ok = readNumber()
			if !ok {
				return false
			}
		}
		if i >= len(format) || !strings.ContainsRune("cdiouxXeEfgGqs", rune(format[i])) {
			return false
		}
		valueSize := 320 // includes the longest finite float64 integer part
		if value, ok := L.Get(argument).(lua.LString); ok {
			valueSize = len(value)*4 + 2
		}
		bound += width + precision + valueSize
		if bound > maxValueBytes {
			return false
		}
		argument++
	}
	return true
}

func boundLibraries(L *lua.LState) {
	allocated, indices := 0, 0
	account := func(L *lua.LState, size int) {
		allocated += size
		if allocated > maxTotal {
			L.RaiseError("metadata allocation limit")
		}
	}
	L.SetGlobal("__dst_metadata_key", L.NewFunction(func(L *lua.LState) int {
		key := L.Get(1)
		if number, ok := key.(lua.LNumber); ok && number >= 1 {
			if number > 16384 {
				L.RaiseError("metadata table index limit")
				return 0
			}
			indices += int(number)
			if indices > 32768 {
				L.RaiseError("metadata table allocation limit")
				return 0
			}
		}
		L.Push(key)
		return 1
	}))
	L.SetGlobal("__dst_metadata_concat", L.NewFunction(func(L *lua.LState) int {
		left, right := L.CheckString(1), L.CheckString(2)
		if len(left)+len(right) > maxValueBytes {
			L.RaiseError("metadata string limit")
			return 0
		}
		account(L, len(left)+len(right))
		L.Push(lua.LString(left + right))
		return 1
	}))
	library := L.GetGlobal("string").(*lua.LTable)
	library.ForEach(func(key, value lua.LValue) {
		fn, ok := value.(*lua.LFunction)
		if !ok || !fn.IsG {
			return
		}
		name := key.String()
		// Native string expansion is bounded before allocation. Pattern replacement
		// callbacks and iterators are unnecessary for display metadata.
		if name == "rep" || name == "gsub" || name == "gmatch" || name == "gfind" {
			library.RawSet(key, lua.LNil)
			return
		}
		library.RawSet(key, L.NewFunction(func(L *lua.LState) int {
			size := 0
			for i := 1; i <= L.GetTop(); i++ {
				if v, ok := L.Get(i).(lua.LString); ok {
					size += len(v)
				}
			}
			if size > maxValueBytes || L.GetTop() > 128 {
				L.RaiseError("metadata string input limit")
				return 0
			}
			if name == "format" && !formatFitsBudget(L) {
				L.RaiseError("metadata format limit")
				return 0
			}
			count := fn.GFunction(L)
			for i := 0; i < count; i++ {
				if value, ok := L.Get(-1 - i).(lua.LString); ok {
					account(L, len(value))
				}
			}
			return count
		}))
	})
	table := L.GetGlobal("table").(*lua.LTable)
	table.RawSetString("concat", L.NewFunction(func(L *lua.LState) int {
		values := L.CheckTable(1)
		sep := L.OptString(2, "")
		start := L.OptInt(3, 1)
		end := L.OptInt(4, values.Len())
		if end-start > 16384 {
			L.RaiseError("metadata table limit")
			return 0
		}
		var out strings.Builder
		for i := start; i <= end; i++ {
			value := values.RawGetInt(i)
			if value.Type() != lua.LTString && value.Type() != lua.LTNumber {
				L.RaiseError("invalid metadata table value")
				return 0
			}
			part := value.String()
			extra := len(part)
			if i > start {
				extra += len(sep)
			}
			if out.Len()+extra > maxValueBytes {
				L.RaiseError("metadata string limit")
				return 0
			}
			if i > start {
				out.WriteString(sep)
			}
			out.WriteString(part)
		}
		account(L, out.Len())
		L.Push(lua.LString(out.String()))
		return 1
	}))
	// table.insert at a huge numeric position would otherwise allocate before
	// the VM can check its context again.
	if insert, ok := table.RawGetString("insert").(*lua.LFunction); ok {
		table.RawSetString("insert", L.NewFunction(func(L *lua.LState) int {
			values := L.CheckTable(1)
			if values.Len() > 16384 || L.GetTop() >= 3 && (L.CheckInt(2) < 1 || L.CheckInt(2) > values.Len()+1) {
				L.RaiseError("metadata table limit")
				return 0
			}
			return insert.GFunction(L)
		}))
	}
}
