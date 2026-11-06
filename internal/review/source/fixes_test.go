package source

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestCalcCyclomaticElseIfAddsOneElseAddsNone(t *testing.T) {
	if got := calcCyclomaticComplexity("if a {} else if b {} else {}"); got != 3 {
		t.Fatalf("cyclomatic = %d, want 3 (base + if + else if)", got)
	}
}

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

func TestCommentedCodePatternSkipsProse(t *testing.T) {
	re := commentedCodePattern()
	for _, line := range []string{"// for each file we walk the tree", "# if the cache is cold we rebuild"} {
		if re.MatchString(line) {
			t.Errorf("prose %q flagged as commented code", line)
		}
	}
	for _, line := range []string{"// if x > 0 {", "# def run(self):"} {
		if !re.MatchString(line) {
			t.Errorf("code %q not flagged", line)
		}
	}
}

func TestPythonPrematureAbstractionIgnoresCommentsAndStrings(t *testing.T) {
	th := ComplexityThresholds{}
	src := "class A:\n    \"\"\"HTTP Protocol helper, ABC\"\"\"\n    # Protocol\n    pass\n"
	for _, f := range analyzePython(src, strings.Count(src, "\n"), th) {
		if f.Rule == "premature-abstraction" {
			t.Fatalf("unexpected finding: %+v", f)
		}
	}
	real := "from typing import Protocol\nclass A(Protocol):\n    pass\n"
	found := false
	for _, f := range analyzePython(real, 3, th) {
		found = found || f.Rule == "premature-abstraction"
	}
	if !found {
		t.Fatal("real Protocol base not flagged")
	}
}
