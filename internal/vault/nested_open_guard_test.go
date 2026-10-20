package vault

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// txOpeners are this package's own transaction-opening functions: a func literal passed as their
// argument is a callback that already runs inside an open transaction/sealed write.
var txOpeners = map[string]bool{"View": true, "ViewAt": true, "Update": true, "UpdateSealed": true}

// bannedInCallback are the calls such a callback must never make directly — each reopens a
// transaction (or MasterKey's own View) and stalls on the outer one's own bbolt flock until its
// Timeout elapses (TestNestedOpenErrors documents the runtime consequence; this is the static check
// the Phase 1/1b review asked for instead).
var bannedInCallback = map[string]bool{
	"View": true, "ViewAt": true, "Update": true, "UpdateSealed": true,
	"MasterKey": true, "EnsureKey": true, "ReadSecret": true, "RotateKey": true, "SnapshotTo": true,
}

// TestNoNestedVaultOpens statically walks every non-test .go file in this package for a func
// literal passed to View/ViewAt/Update/UpdateSealed and fails if that literal's own body calls a
// transaction-opening function directly (package-local, unqualified calls only — a transaction-local
// helper taking *Tx/*Key is not itself one of these names, so it is never flagged).
func TestNoNestedVaultOpens(t *testing.T) {
	for _, v := range findNestedOpens(t, ".") {
		t.Error(v)
	}
}

// findNestedOpens is split out so TestNoNestedVaultOpensCatchesIntroducedNesting can run it against
// a scratch copy with a deliberately injected nested call, proving the check has teeth.
func findNestedOpens(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || !txOpeners[ident.Name] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				ast.Inspect(lit.Body, func(inner ast.Node) bool {
					innerCall, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					if innerIdent, ok := innerCall.Fun.(*ast.Ident); ok && bannedInCallback[innerIdent.Name] {
						pos := fset.Position(innerCall.Pos())
						violations = append(violations, fmt.Sprintf(
							"%s:%d: a %s callback calls %s directly, nesting a vault transaction",
							pos.Filename, pos.Line, ident.Name, innerIdent.Name))
					}
					return true
				})
			}
			return true
		})
	}
	return violations
}

// TestNoNestedVaultOpensCatchesIntroducedNesting proves findNestedOpens has teeth: it injects a
// nested View() call into a scratch copy of an existing Update callback and confirms the check
// flags it, instead of trusting an untested detector.
func TestNoNestedVaultOpensCatchesIntroducedNesting(t *testing.T) {
	src, err := os.ReadFile("key.go")
	if err != nil {
		t.Fatal(err)
	}
	anchor := "id = vaultID(t)\n\t\treturn nil\n"
	if n := strings.Count(string(src), anchor); n != 1 {
		t.Fatalf("anchor appears %d times in key.go, want exactly 1 (source may have shifted)", n)
	}
	injected := strings.Replace(string(src), anchor,
		"id = vaultID(t)\n\t\tView(func(*Tx) error { return nil })\n\t\treturn nil\n", 1)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.go"), []byte(injected), 0600); err != nil {
		t.Fatal(err)
	}
	if violations := findNestedOpens(t, dir); len(violations) == 0 {
		t.Fatal("injected nested View() inside an Update callback was not detected — the guard has no teeth")
	}
}
