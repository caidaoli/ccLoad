package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestAdminHandlers_DoNotUseGinJSONDirectly(t *testing.T) {
	t.Helper()

	// 这些调用会绕过 APIResponse 统一格式（success/data/error/count）。
	banned := map[string]bool{
		"JSON":                true,
		"IndentedJSON":        true,
		"SecureJSON":          true,
		"AsciiJSON":           true,
		"PureJSON":            true,
		"JSONP":               true,
		"AbortWithStatusJSON": true,
	}

	var files []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if strings.HasPrefix(name, "admin_") {
			files = append(files, name)
		}
	}

	// RequireTokenAuth 属于 Admin API 认证链路；RequireAPIAuth 属于代理API（不强制APIResponse格式）。
	files = append(files, "auth_service.go")
	sort.Strings(files)

	var offenders []string
	for _, filename := range files {
		allowInFunc := map[string]bool{}
		if filename == "auth_service.go" {
			allowInFunc["RequireAPIAuth"] = true
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", filename, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			funcName := ""
			if fn.Name != nil {
				funcName = fn.Name.Name
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					return true
				}
				if !banned[sel.Sel.Name] {
					return true
				}
				if allowInFunc[funcName] {
					return true
				}
				pos := fset.Position(sel.Sel.Pos())
				offenders = append(offenders, fmt.Sprintf("%s:%d:%d %s.%s()", filename, pos.Line, pos.Column, funcName, sel.Sel.Name))
				return true
			})
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("发现绕过APIResponse的直接JSON输出（请改用 RespondJSON/RespondError*）：\n- %s", strings.Join(offenders, "\n- "))
	}
}
