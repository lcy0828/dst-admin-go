package modmetadata

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

const maxValueBytes = 64 << 10

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
		return context.DeadlineExceeded
	}
	return b.Context.Err()
}
func withInstructionBudget(ctx context.Context) context.Context {
	exhausted := make(chan struct{})
	close(exhausted)
	return &evaluationBudget{Context: ctx, remaining: 20000, exhausted: exhausted}
}

// Replacing concatenation before compilation catches both local and global
// strings, including concatenation in dependencies and nested functions.
func boundedLoad(L *lua.LState, data []byte, path string) (*lua.LFunction, error) {
	chunk, err := parse.Parse(bytes.NewReader(data), path)
	if err != nil {
		return nil, err
	}
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
				access.Key = &ast.FuncCallExpr{Func: &ast.IdentExpr{Value: "__dst_metadata_key"}, Args: []ast.Expr{key}}
				walk(reflect.ValueOf(&access.Object).Elem())
				return
			}
			if table, ok := v.Interface().(*ast.TableExpr); ok {
				for _, field := range table.Fields {
					walk(reflect.ValueOf(&field.Value).Elem())
					if field.Key != nil {
						walk(reflect.ValueOf(&field.Key).Elem())
						field.Key = &ast.FuncCallExpr{Func: &ast.IdentExpr{Value: "__dst_metadata_key"}, Args: []ast.Expr{field.Key}}
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

var formatNumber = regexp.MustCompile(`[0-9]{5,}`)

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
			if name == "format" && (len(L.CheckString(1)) > 1024 || formatNumber.MatchString(L.CheckString(1))) {
				L.RaiseError("metadata format limit")
				return 0
			}
			// Each format specifier may expand to its width/precision or full argument.
			if name == "format" && strings.Count(L.CheckString(1), "%") > 8 {
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
