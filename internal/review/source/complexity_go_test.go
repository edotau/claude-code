package source

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestGoNestingElseIfChainIsFlat(t *testing.T) {
	src := "package p\nfunc f(a, b, c, d, e bool) {\nif a {} else if b {} else if c {} else if d {} else if e {} else {}\n}\n"
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := file.Decls[0].(*ast.FuncDecl).Body
	if got := goNesting(body, 0); got != 1 {
		t.Fatalf("nesting = %d, want 1", got)
	}
	nested := "package p\nfunc f(a, b bool) {\nif a {} else if b { for { if a {} } }\n}\n"
	file, _ = parser.ParseFile(token.NewFileSet(), "p.go", nested, 0)
	if got := goNesting(file.Decls[0].(*ast.FuncDecl).Body, 0); got != 3 {
		t.Fatalf("nested nesting = %d, want 3", got)
	}
}
