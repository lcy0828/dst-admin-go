package mods

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

// File editors can preserve a literal table semantically. Evaluating dynamic
// code first would hide duplicate keys and discard its behavior on save.
func validateLiteralOverrides(data []byte, name string) error {
	if int64(len(data)) > maxLuaBytes {
		return fmt.Errorf("Lua file exceeds 4 MiB")
	}
	chunk, err := parse.Parse(bytes.NewReader(data), name)
	if err != nil {
		return err
	}
	if len(chunk) != 1 {
		return fmt.Errorf("%s must contain a literal return table; dynamic configuration is preserved without editing", name)
	}
	statement, ok := chunk[0].(*ast.ReturnStmt)
	if !ok || len(statement.Exprs) != 1 {
		return fmt.Errorf("%s must return one literal table", name)
	}
	if _, ok := statement.Exprs[0].(*ast.TableExpr); !ok {
		return fmt.Errorf("%s must return a literal table", name)
	}
	count := 0
	return validateOverrideExpression(statement.Exprs[0], 0, &count)
}

func validateOverrideExpression(expression ast.Expr, depth int, count *int) error {
	if depth > maxLuaDepth {
		return fmt.Errorf("%w: nesting exceeds %d", ErrUnsupportedLuaValue, maxLuaDepth)
	}
	switch value := expression.(type) {
	case *ast.StringExpr, *ast.NumberExpr, *ast.TrueExpr, *ast.FalseExpr, *ast.NilExpr:
		return nil
	case *ast.UnaryMinusOpExpr:
		if _, ok := value.Expr.(*ast.NumberExpr); ok {
			return nil
		}
	case *ast.TableExpr:
		keys := make(map[string]bool, len(value.Fields))
		arrayIndex := 0
		for _, field := range value.Fields {
			*count++
			if *count > maxLuaEntries {
				return fmt.Errorf("%w: table exceeds %d entries", ErrUnsupportedLuaValue, maxLuaEntries)
			}
			key := ""
			if field.Key == nil {
				arrayIndex++
				key = "n:" + strconv.Itoa(arrayIndex)
			} else {
				var err error
				key, err = overrideLiteralKey(field.Key)
				if err != nil {
					return err
				}
			}
			if keys[key] {
				return fmt.Errorf("%w: duplicate Lua table key %s; refusing a lossy edit", ErrUnsupportedLuaValue, key)
			}
			keys[key] = true
			if err := validateOverrideExpression(field.Value, depth+1, count); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%w: dynamic Lua expressions cannot be edited without losing their behavior", ErrUnsupportedLuaValue)
}

func overrideLiteralKey(expression ast.Expr) (string, error) {
	switch value := expression.(type) {
	case *ast.StringExpr:
		return "s:" + value.Value, nil
	case *ast.TrueExpr:
		return "b:true", nil
	case *ast.FalseExpr:
		return "b:false", nil
	case *ast.NumberExpr:
		number, err := strconv.ParseFloat(value.Value, 64)
		if err != nil {
			integer, intErr := strconv.ParseInt(value.Value, 0, 64)
			if intErr != nil {
				return "", err
			}
			number = float64(integer)
		}
		return "n:" + strconv.FormatFloat(number, 'g', -1, 64), nil
	case *ast.UnaryMinusOpExpr:
		key, err := overrideLiteralKey(value.Expr)
		if err == nil && len(key) > 2 && key[:2] == "n:" {
			number, _ := strconv.ParseFloat(key[2:], 64)
			if number == 0 {
				return "n:0", nil
			}
			return "n:" + strconv.FormatFloat(-number, 'g', -1, 64), nil
		}
	}
	return "", fmt.Errorf("%w: dynamic Lua table key", ErrUnsupportedLuaValue)
}
