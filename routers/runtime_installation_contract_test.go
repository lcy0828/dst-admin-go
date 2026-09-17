package routers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestRouterDoesNotAutomaticallyInstallManagedLuaRuntime(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "router.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "InstallRoom" || selector.Sel.Name == "InstallWorld" {
			t.Errorf("router construction and room lifecycle must not call %s; Runtime installation is an explicit API action", selector.Sel.Name)
		}
		return true
	})
}
