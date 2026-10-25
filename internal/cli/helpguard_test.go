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

// callsHelptext recognizes helptext.Fprint and helptext.Page receivers' Fprint calls.
func callsHelptext(n ast.Node, pages map[string]ast.Expr) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok {
			if isSel(c.Fun, "helptext", "Fprint") {
				found = true
			} else if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Fprint" {
				found = isHelpPage(s.X, pages, 0)
			}
		}
		return !found
	})
	return found
}

func isHelpPage(e ast.Expr, pages map[string]ast.Expr, depth int) bool {
	if depth > 10 || e == nil {
		return false
	}
	switch x := e.(type) {
	case *ast.CompositeLit:
		return isSel(x.Type, "helptext", "Page")
	case *ast.ParenExpr:
		return isHelpPage(x.X, pages, depth+1)
	case *ast.UnaryExpr:
		return isHelpPage(x.X, pages, depth+1)
	case *ast.Ident:
		if x.Obj != nil {
			switch d := x.Obj.Decl.(type) {
			case *ast.AssignStmt:
				for i, lhs := range d.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == x.Name && i < len(d.Rhs) {
						return isHelpPage(d.Rhs[i], pages, depth+1)
					}
				}
			case *ast.ValueSpec:
				if isSel(d.Type, "helptext", "Page") {
					return true
				}
				for i, name := range d.Names {
					if name.Name == x.Name && i < len(d.Values) {
						return isHelpPage(d.Values[i], pages, depth+1)
					}
				}
			case *ast.Field:
				return isSel(d.Type, "helptext", "Page")
			}
		}
		return isHelpPage(pages[x.Name], pages, depth+1)
	}
	return false
}

func pageDecls(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, name := range vs.Names {
							if isSel(vs.Type, "helptext", "Page") {
								out[name.Name] = &ast.CompositeLit{Type: vs.Type}
							} else if i < len(vs.Values) {
								out[name.Name] = vs.Values[i]
							}
						}
					}
				}
			}
		}
	}
	return out
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

// funcsCallingHelptext names package-level functions that render shared help.
func funcsCallingHelptext(files []*ast.File, pages map[string]ast.Expr) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil && callsHelptext(fd.Body, pages) {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

// usageAssigned requires a renderer on this FlagSet, not another one in the same function.
func usageAssigned(body ast.Node, flagset string, themedFuncs map[string]bool, pages map[string]ast.Expr) bool {
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
			id, isID := sel.X.(*ast.Ident)
			if !isID || flagset == "" || id.Name != flagset {
				continue
			}
			switch r := as.Rhs[i].(type) {
			case *ast.FuncLit:
				ok = ok || callsHelptext(r, pages)
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
		f, perr := parser.ParseFile(fset, p, nil, 0)
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
		pages := pageDecls(files)
		decls, themed := strDecls(files), funcsCallingHelptext(files, pages)
		for _, f := range files {
			allowed := themedAllowlist[f.Name.Name] != ""
			out = append(out, scanFile(fset, f, decls, themed, pages, allowed)...)
		}
	}
	return out
}

func scanFile(fset *token.FileSet, f *ast.File, decls map[string]string, themed map[string]bool, pages map[string]ast.Expr, allowFlagSets bool) []guardFinding {
	var out []guardFinding
	add := func(n ast.Node, msg string) {
		out = append(out, guardFinding{fset.Position(n.Pos()).String(), msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, _ := strconv.Unquote(lit.Value); strings.Contains(s, "Usage of") {
				add(lit, `legacy "Usage of" string literal: use helptext.Page`)
			}
		}
		return true
	})
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		flagsets := map[*ast.CallExpr]string{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range d.Rhs {
					if c, ok := rhs.(*ast.CallExpr); ok && i < len(d.Lhs) {
						if id, ok := d.Lhs[i].(*ast.Ident); ok {
							flagsets[c] = id.Name
						}
					}
				}
			case *ast.ValueSpec:
				for i, value := range d.Values {
					if c, ok := value.(*ast.CallExpr); ok && i < len(d.Names) {
						flagsets[c] = d.Names[i].Name
					}
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if isSel(c.Fun, "flag", "NewFlagSet") && !allowFlagSets && !usageAssigned(fd.Body, flagsets[c], themed, pages) {
				add(c, "flag.NewFlagSet without shared help: use helptext.NewFlagSet or assign Usage calling helptext.Page.Fprint/helptext.Fprint")
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

// TestHelpGuard enforces shared help renderers and rejects legacy headers and raw usage writes.
func TestHelpGuard(t *testing.T) {
	for _, f := range scanHelpGuard(t, "../..") {
		t.Errorf("%s: %s", f.pos, f.msg)
	}
}

func TestHelpGuardFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		findings     int
	}{
		{"constructor", `func run() { fs := helptext.NewFlagSet("demo", helptext.Page{}); _ = fs }`, 0},
		{"function renderer", `func run() {
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.Usage = func() { helptext.Fprint(fs.Output(), "usage: claude-code demo") }
		}`, 0},
		{"page renderer", `func run() {
			p := helptext.Page{Usage: "claude-code demo"}
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.Usage = func() { p.Fprint(fs.Output()) }
		}`, 0},
		{"inline page renderer", `func run() {
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.Usage = func() { (helptext.Page{}).Fprint(fs.Output()) }
		}`, 0},
		{"named renderer", `var page = helptext.Page{}
		func render() { page.Fprint(os.Stderr) }
		func run() {
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.Usage = render
		}`, 0},
		{"unwired", `func run() { fs := flag.NewFlagSet("demo", flag.ContinueOnError); _ = fs }`, 1},
		{"unrelated renderer", `func run() {
			fs := flag.NewFlagSet("demo", flag.ContinueOnError)
			fs.Usage = func() { other.Fprint(fs.Output()) }
		}`, 1},
		{"other flagset wired", `func run() {
			first := flag.NewFlagSet("first", flag.ContinueOnError)
			second := flag.NewFlagSet("second", flag.ContinueOnError)
			first.Usage = func() { helptext.Fprint(first.Output(), "help") }
			_ = second
		}`, 1},
		{"legacy literal outside function", `const text = "Usage of demo:"`, 1},
		{"legacy literal inside function", `func run() { println("Usage of demo:") }`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", "package fixture\n"+tc.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			files := []*ast.File{f}
			pages := pageDecls(files)
			got := scanFile(fset, f, strDecls(files), funcsCallingHelptext(files, pages), pages, false)
			if len(got) != tc.findings {
				t.Errorf("findings = %v, want %d", got, tc.findings)
			}
		})
	}
}
