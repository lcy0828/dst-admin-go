package mods

import (
	"bytes"

	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

// Lists only need literal identity fields. Executing a Mod and its configuration
// helpers belongs to the configuration editor, not to a content inventory read.
func literalModMetadata(path string) map[string]interface{} {
	data, _, _, err := readModFile(path, false)
	if err != nil {
		return nil
	}
	statements, err := parse.Parse(bytes.NewReader(data), "modinfo.lua")
	if err != nil {
		return nil
	}
	values := make(map[string]interface{})
	for _, statement := range statements {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for index, expression := range assignment.Lhs {
			name, ok := expression.(*ast.IdentExpr)
			if !ok || index >= len(assignment.Rhs) {
				continue
			}
			switch name.Value {
			case "name", "version", "author", "description":
				delete(values, name.Value)
				if value, ok := assignment.Rhs[index].(*ast.StringExpr); ok {
					values[name.Value] = value.Value
				}
			}
		}
	}
	return values
}
