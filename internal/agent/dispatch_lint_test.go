package agent_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Guarantee 4: the dispatch path never mutates process-global env or cwd (concurrent tasks would race on it).
func TestNoSetenvOrChdirInDispatchPath(t *testing.T) {
	banned := map[string]bool{"os.Setenv": true, "os.Unsetenv": true, "os.Clearenv": true, "os.Chdir": true}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name+"."+sel.Sel.Name] {
					t.Errorf("%s: %s.%s in the dispatch path: pass env/dir through childCmd instead", p, id.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
