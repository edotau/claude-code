package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// themedAllowlist: packages whose FlagSets are built by a caller that already wired themed help.
var themedAllowlist = map[string]string{
	// statusline.Run takes the caller's FlagSet (internal/cli/theme.go builds it with helptext.NewFlagSet).
	"statusline": "FlagSet is supplied by the caller, already themed",
	// helptext.NewFlagSet is the constructor itself.
	"helptext": "defines NewFlagSet",
}

// guardFinding is one violation of the help-text guard.
type guardFinding struct{ pos, msg string }

func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != name {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// callsHelptext reports whether n contains a helptext.Fprint or helptext.Defaults call.
func callsHelptext(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok && (isSel(c.Fun, "helptext", "Fprint") || isSel(c.Fun, "helptext", "Defaults")) {
			found = true
		}
		return !found
	})
	return found
}

// strDecls collects package-level string const/var literals by name, per directory.
func strDecls(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						out[n.Name] = firstString(vs.Values[i])
					}
				}
			}
		}
	}
	return out
}

// firstString returns the leftmost string literal inside e ("" when none).
func firstString(e ast.Expr) string {
	s := ""
	ast.Inspect(e, func(x ast.Node) bool {
		if s != "" {
			return false
		}
		if l, ok := x.(*ast.BasicLit); ok && l.Kind == token.STRING {
			s, _ = strconv.Unquote(l.Value)
			return false
		}
		return true
	})
	return s
}

// funcsCallingHelptext names the package-level functions that call helptext.Fprint/Defaults.
func funcsCallingHelptext(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && callsHelptext(fd.Body) {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

// usageAssigned reports whether body assigns <x>.Usage a themed func (literal or named).
func usageAssigned(body ast.Node, themedFuncs map[string]bool) bool {
	ok := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, isAs := n.(*ast.AssignStmt)
		if !isAs {
			return true
		}
		for i, l := range as.Lhs {
			sel, isSel := l.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "Usage" || i >= len(as.Rhs) {
				continue
			}
			switch r := as.Rhs[i].(type) {
			case *ast.FuncLit:
				ok = ok || callsHelptext(r)
			case *ast.Ident:
				ok = ok || themedFuncs[r.Name]
			}
		}
		return true
	})
	return ok
}

var flagDefMethods = map[string]int{ // method -> index of the usage argument
	"Bool": 2, "String": 2, "Int": 2, "Int64": 2, "Uint": 2, "Uint64": 2, "Duration": 2, "Float64": 2, "Var": 2,
	"BoolVar": 3, "StringVar": 3, "IntVar": 3, "Int64Var": 3, "UintVar": 3, "Uint64Var": 3, "DurationVar": 3, "Float64Var": 3,
}

// scanHelpGuard checks every non-test .go file under root and returns the violations.
func scanHelpGuard(t *testing.T, root string) []guardFinding {
	t.Helper()
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []guardFinding
	for _, files := range byDir {
		decls, themed := strDecls(files), funcsCallingHelptext(files)
		for _, f := range files {
			allowed := themedAllowlist[f.Name.Name] != ""
			out = append(out, scanFile(fset, f, decls, themed, allowed)...)
		}
	}
	return out
}

func scanFile(fset *token.FileSet, f *ast.File, decls map[string]string, themed map[string]bool, allowFlagSets bool) []guardFinding {
	var out []guardFinding
	add := func(n ast.Node, msg string) {
		out = append(out, guardFinding{fset.Position(n.Pos()).String(), msg})
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isSel(c.Fun, "flag", "NewFlagSet") && !allowFlagSets && !usageAssigned(fd.Body, themed) {
				add(c, "flag.NewFlagSet without a themed Usage: use helptext.NewFlagSet or assign Usage calling helptext.Fprint/Defaults")
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				if idx, isDef := flagDefMethods[sel.Sel.Name]; isDef && len(c.Args) > idx {
					if lit, isLit := c.Args[idx].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
						if s, _ := strconv.Unquote(lit.Value); strings.Contains(s, "`") {
							add(c, "flag usage string contains a backtick (flag renames the value type after it): "+s)
						}
					}
				}
			}
			if isSel(c.Fun, "fmt", "Fprint") || isSel(c.Fun, "fmt", "Fprintln") || isSel(c.Fun, "fmt", "Fprintf") {
				if len(c.Args) >= 2 && isSel(c.Args[0], "os", "Stderr") && usageText(c.Args[1], decls) {
					add(c, `plain fmt write of a "usage:" string to os.Stderr: use helptext.Fprint`)
				}
			}
			return true
		})
	}
	return out
}

func usageText(e ast.Expr, decls map[string]string) bool {
	if id, ok := e.(*ast.Ident); ok {
		return strings.HasPrefix(decls[id.Name], "usage:")
	}
	return strings.HasPrefix(firstString(e), "usage:")
}

// TestHelpGuard: every FlagSet is themed, no flag usage carries a backtick, no raw "usage:" to stderr.
func TestHelpGuard(t *testing.T) {
	for _, f := range scanHelpGuard(t, "..") {
		t.Errorf("%s: %s", f.pos, f.msg)
	}
}
