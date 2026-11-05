package settings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRenderHasOnlySeedAndResetCallers: any production Render reference beyond the seed and the reset is a new writer.
func TestRenderHasOnlySeedAndResetCallers(t *testing.T) {
	root := filepath.Join("..", "..")
	var got []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return skipDir(err, d, path != root)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		got = append(got, renderRefs(t, path, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"internal/settings/apply.go:ResetSettings", "internal/settings/apply.go:SeedSettings"}
	if !slices.Equal(got, want) {
		t.Errorf("Render referenced from %v, want only %v", got, want)
	}
}

// skipDir passes a walk error through and skips testdata and dot directories below the root.
func skipDir(err error, d fs.DirEntry, below bool) error {
	if err == nil && below && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
		return filepath.SkipDir
	}
	return err
}

// renderRefs lists file:func for every reference to settings.Render (or bare Render inside package settings).
func renderRefs(t *testing.T, path, rel string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	inPkg := f.Name.Name == "settings" && strings.HasPrefix(rel, "internal/settings/")
	var refs []string
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && refersToRender(fn.Body, inPkg) {
			refs = append(refs, rel+":"+fn.Name.Name)
		}
	}
	return refs
}

// refersToRender reports a settings.Render selector, or a bare Render ident when inPkg, anywhere under body.
func refersToRender(body ast.Node, inPkg bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			found = found || ok && id.Name == "settings" && x.Sel.Name == "Render"
			return false
		case *ast.Ident:
			found = found || inPkg && x.Name == "Render"
		}
		return !found
	})
	return found
}
