package helptext

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestPageLayout(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Bool("dry-run", false, "print only; no-op for --check and e-mail-style text")
	fs.String("format", "text", "output `kind`: text|json")
	fs.Int("n", 0, "count")
	fs.String("out", "", "destination")
	fs.Duration("timeout", 0, "wait duration")
	fs.String("very-long-flag-name-for-the-label", "", "long label description")
	p := Page{
		Usage:    "claude-code demo [flags]",
		About:    strings.Repeat("A descriptive sentence about this command. ", 5),
		Commands: [][2]string{{"show", "show the current configuration"}},
		Flags:    fs,
		Hint:     "claude-code demo show -h lists that page's flags.\n",
	}
	p.Fprint(&buf)
	got := buf.String()
	if !strings.HasPrefix(got, "usage: claude-code demo [flags]\n\n") {
		t.Errorf("missing opening synopsis:\n%s", got)
	}
	assertContainsAll(t, got, []string{
		"\n\nCOMMANDS\n", "\n\nFLAGS\n", "  show", "--format kind",
		`output kind: text|json (default "text")`, "no-op for --check and e-mail-style",
	})
	if !strings.Contains(strings.Join(strings.Fields(got), " "), "no-op for --check and e-mail-style text") {
		t.Errorf("description changed while wrapping:\n%s", got)
	}
	fs.VisitAll(func(f *flag.Flag) { assertOneFlagRow(t, got, f) })
	if !strings.Contains(got, "--very-long-flag-name-for-the-label string  long label description") {
		t.Errorf("overlong label lacks two-space separator:\n%s", got)
	}
	assertMaxColumns(t, got, 80)
	about := strings.Split(got, "\n\n")[1]
	if strings.Join(strings.Fields(about), " ") != strings.TrimSpace(p.About) {
		t.Errorf("About changed while wrapping: %q", about)
	}
	if !strings.HasSuffix(got, "\n\n"+p.Hint) {
		t.Errorf("Hint is not the last section:\n%s", got)
	}
}

func assertContainsAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// assertOneFlagRow requires the flag f to head exactly one row of the rendered page got.
func assertOneFlagRow(t *testing.T, got string, f *flag.Flag) {
	t.Helper()
	count := 0
	label := "--" + f.Name
	if utf8.RuneCountInString(f.Name) == 1 {
		label = "-" + f.Name
	}
	for line := range strings.SplitSeq(got, "\n") {
		words := strings.Fields(line)
		if len(words) == 0 || words[0] != label {
			continue
		}
		count++
		if f.Name != "format" && strings.Contains(line, "(default") {
			t.Errorf("zero-valued flag has a default: %q", line)
		}
	}
	if count != 1 {
		t.Errorf("--%s has %d rows, want 1", f.Name, count)
	}
}

func assertMaxColumns(t *testing.T, got string, max int) {
	t.Helper()
	for line := range strings.SplitSeq(got, "\n") {
		if n := utf8.RuneCountInString(line); n > max {
			t.Errorf("line has %d columns: %q", n, line)
		}
	}
}

func TestPageOmitsEmptySections(t *testing.T) {
	var out bytes.Buffer
	Page{Usage: "claude-code demo"}.Fprint(&out)
	if got, want := out.String(), "usage: claude-code demo\n"; got != want {
		t.Errorf("page = %q, want %q", got, want)
	}
}

func TestNewFlagSetParse(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		name string
		args []string
		code int
		done bool
	}{
		{"short help", []string{"-h"}, 0, true},
		{"long help", []string{"--help"}, 0, true},
		{"bad flag", []string{"--missing"}, 2, true},
		{"bad value", []string{"--wait=invalid"}, 2, true},
		{"valid", []string{"--quiet"}, 0, false},
		{"empty", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := NewFlagSet("demo", Page{Usage: "claude-code demo [flags]"})
			fs.Bool("quiet", false, "less output")
			fs.Duration("wait", time.Second, "wait duration")
			var buf bytes.Buffer
			fs.SetOutput(&buf)
			if fs.ErrorHandling() != flag.ContinueOnError {
				t.Errorf("ErrorHandling = %v, want ContinueOnError", fs.ErrorHandling())
			}
			if code, done := Parse(fs, tc.args); code != tc.code || done != tc.done {
				t.Errorf("Parse = (%d, %t), want (%d, %t)", code, done, tc.code, tc.done)
			}
			if tc.done && !strings.Contains(buf.String(), "usage: claude-code demo [flags]") {
				t.Errorf("parse omitted help:\n%s", &buf)
			}
			if tc.code == 0 && tc.done && strings.Count(buf.String(), "--quiet") != 1 {
				t.Errorf("help must list each registered flag once:\n%s", &buf)
			}
		})
	}
}

// placeholderValue is a custom flag.Value implementing Placeholder, so its FLAGS row names its own type.
type placeholderValue string

func (p *placeholderValue) String() string      { return string(*p) }
func (p *placeholderValue) Set(s string) error  { *p = placeholderValue(s); return nil }
func (p *placeholderValue) Placeholder() string { return "KEY,KEY" }

// plainValue is a custom flag.Value with no Placeholder hook: flag.UnquoteUsage's generic fallback applies.
type plainValue string

func (p *plainValue) String() string     { return string(*p) }
func (p *plainValue) Set(s string) error { *p = plainValue(s); return nil }

// A custom Value implementing Placeholder renders that placeholder in FLAGS; one without it falls back
// to flag.UnquoteUsage's generic "value" (no type name, no backtick in the usage string).
func TestFlagRowsPlaceholderHook(t *testing.T) {
	var withHook placeholderValue
	var without plainValue
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Var(&withHook, "drop", "names to drop")
	fs.Var(&without, "keep", "names to keep")
	rows := flagRows(fs)
	got := map[string]string{}
	for _, r := range rows {
		got[r[0]] = r[1]
	}
	if _, ok := got["--drop KEY,KEY"]; !ok {
		t.Errorf("want a --drop KEY,KEY row from the Placeholder hook, got rows: %v", rows)
	}
	if _, ok := got["--keep value"]; !ok {
		t.Errorf("want a --keep value row (no Placeholder hook), got rows: %v", rows)
	}
}

// A one-letter "shorthand for --X" flag folds into X's row; an orphan shorthand keeps its own row.
func TestFlagRowsFoldShorthand(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	p := fs.String("profile", "", "the profile")
	fs.StringVar(p, "p", "", "shorthand for --profile")
	fs.String("q", "", "shorthand for --missing")
	got := fmt.Sprint(flagRows(fs))
	want := "[[-p, --profile string the profile] [-q string shorthand for --missing]]"
	if got != want {
		t.Errorf("rows = %s, want %s", got, want)
	}
}

// Fprint paints only on a terminal; Paint is the forced path NewFlagSet's Usage goes through.
func TestPaintColorsDoubleDashFlagRows(t *testing.T) {
	painted := Paint("FLAGS\n  --quiet\n    \tless output\n")
	if !strings.Contains(painted, "\x1b[") {
		t.Errorf("Paint added no escape codes: %q", painted)
	}
	if !strings.Contains(painted, "--quiet") {
		t.Errorf("painted row lost its label: %q", painted)
	}
}

// WrappedDocs runs `<bin> --help` from PATH and passes its exit code through; a missing binary is exit 1.
func TestWrappedDocsRunsBinaryHelp(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$@\" > " + out + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "fakecli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if code := WrappedDocs("fakecli"); code != 3 {
		t.Fatalf("exit = %d, want the binary's 3", code)
	}
	if got, _ := os.ReadFile(out); string(got) != "--help\n" {
		t.Fatalf("argv = %q, want --help", got)
	}
	if code := WrappedDocs("no-such-cli"); code != 1 {
		t.Fatalf("missing binary exit = %d, want 1", code)
	}
}

// `<bin> --help` holds no login or credential-shaped var, but keeps PATH and the proxy/CA settings.
func TestWrappedDocsScrubsCredentials(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	script := "#!/bin/sh\n/usr/bin/env > " + dump + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "fakecli"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	gone := []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "DATABRICKS_TOKEN"}
	kept := []string{"HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE"}
	for _, k := range append(slices.Clone(gone), kept...) {
		t.Setenv(k, "SENTINEL")
	}
	if code := WrappedDocs("fakecli"); code != 3 {
		t.Fatalf("exit = %d, want the binary's 3", code)
	}
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range gone {
		if strings.Contains("\n"+string(env), "\n"+k+"=") {
			t.Errorf("--help inherited %s", k)
		}
	}
	for _, k := range append(kept, "PATH") {
		if !strings.Contains("\n"+string(env), "\n"+k+"=") {
			t.Errorf("--help lost %s", k)
		}
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func lead(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func TestWrapRow(t *testing.T) {
	const lw, w = 18, 80
	long := strings.Repeat("alpha beta gamma ", 12)
	tests := []struct {
		name, desc string
		wantLines  int // 0 = just "more than one"
	}{
		{"short", "one short description", 1},
		{"empty", "", 1},
		{"long", long, 0},
		{"overlong word", "see " + strings.Repeat("x", 90) + " now", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapRow("  name", tc.desc, lw, w)
			switch {
			case tc.wantLines > 0 && len(got) != tc.wantLines:
				t.Fatalf("lines = %d, want %d: %q", len(got), tc.wantLines, got)
			case tc.wantLines == 0 && len(got) < 2:
				t.Fatalf("want a wrap, got %q", got)
			}
			if !strings.HasPrefix(got[0], "  name"+strings.Repeat(" ", lw-6)) {
				t.Errorf("first line label not padded to %d: %q", lw, got[0])
			}
			assertWrapContinuations(t, got, tc.desc, lw, w)
		})
	}
	if got := wrapRow("  name", "one short description", lw, w); got[0] != "  name"+strings.Repeat(" ", 12)+"one short description" {
		t.Errorf("short row changed: %q", got[0])
	}
}

// assertWrapContinuations checks every wrapped line of got keeps the label column and the description's words.
func assertWrapContinuations(t *testing.T, got []string, desc string, lw, w int) {
	t.Helper()
	var words []string
	for i, l := range got {
		if i > 0 && lead(l) != lw {
			t.Errorf("continuation %d starts at col %d, want %d: %q", i, lead(l), lw, l)
		}
		if utf8.RuneCountInString(l) > w && len(strings.Fields(l[lw:])) > 1 {
			t.Errorf("line %d is %d cols with several words: %q", i, utf8.RuneCountInString(l), l)
		}
		words = append(words, strings.Fields(l[min(lw, len(l)):])...)
	}
	if strings.Join(words, " ") != strings.Join(strings.Fields(desc), " ") {
		t.Errorf("words changed or split: %q", words)
	}
}

func TestPaintKeepsWrappedLayout(t *testing.T) {
	text := strings.Join(wrapRow("  name", strings.Repeat("alpha beta ", 12), 18, 80), "\n") + "\n"
	if got := ansiRE.ReplaceAllString(Paint(text), ""); got != text {
		t.Errorf("plain text of Paint differs:\n%q\nwant\n%q", got, text)
	}
}

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
		return identIsHelpPage(x, pages, depth)
	}
	return false
}

// identIsHelpPage resolves an identifier through its declaration, then through the package-level pages.
func identIsHelpPage(x *ast.Ident, pages map[string]ast.Expr, depth int) bool {
	if x.Obj != nil {
		if r, decided := declIsHelpPage(x, pages, depth); decided {
			return r
		}
	}
	return isHelpPage(pages[x.Name], pages, depth+1)
}

// declIsHelpPage answers from x's declaration; decided is false when the declaration says nothing about x.
func declIsHelpPage(x *ast.Ident, pages map[string]ast.Expr, depth int) (result, decided bool) {
	switch d := x.Obj.Decl.(type) {
	case *ast.AssignStmt:
		return assignIsHelpPage(d, x, pages, depth)
	case *ast.ValueSpec:
		return specIsHelpPage(d, x, pages, depth)
	case *ast.Field:
		return isSel(d.Type, "helptext", "Page"), true
	}
	return false, false
}

func assignIsHelpPage(d *ast.AssignStmt, x *ast.Ident, pages map[string]ast.Expr, depth int) (result, decided bool) {
	for i, lhs := range d.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && id.Name == x.Name && i < len(d.Rhs) {
			return isHelpPage(d.Rhs[i], pages, depth+1), true
		}
	}
	return false, false
}

func specIsHelpPage(d *ast.ValueSpec, x *ast.Ident, pages map[string]ast.Expr, depth int) (result, decided bool) {
	if isSel(d.Type, "helptext", "Page") {
		return true, true
	}
	for i, name := range d.Names {
		if name.Name == x.Name && i < len(d.Values) {
			return isHelpPage(d.Values[i], pages, depth+1), true
		}
	}
	return false, false
}

// valueSpecs lists every package-level const/var spec of files, in source order.
func valueSpecs(files []*ast.File) []*ast.ValueSpec {
	var out []*ast.ValueSpec
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					out = append(out, vs)
				}
			}
		}
	}
	return out
}

func pageDecls(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, vs := range valueSpecs(files) {
		for i, name := range vs.Names {
			if isSel(vs.Type, "helptext", "Page") {
				out[name.Name] = &ast.CompositeLit{Type: vs.Type}
			} else if i < len(vs.Values) {
				out[name.Name] = vs.Values[i]
			}
		}
	}
	return out
}

// strDecls collects package-level string const/var literals by name, per directory.
func strDecls(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, vs := range valueSpecs(files) {
		for i, n := range vs.Names {
			if i < len(vs.Values) {
				out[n.Name] = firstString(vs.Values[i])
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
		if as, isAs := n.(*ast.AssignStmt); isAs {
			for i := range as.Lhs {
				ok = usageRendererAt(as, i, flagset, themedFuncs, pages) || ok
			}
		}
		return true
	})
	return ok
}

// usageRendererAt reports whether as.Lhs[i] is flagset.Usage assigned a function that renders shared help.
func usageRendererAt(as *ast.AssignStmt, i int, flagset string, themedFuncs map[string]bool, pages map[string]ast.Expr) bool {
	sel, isSel := as.Lhs[i].(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Usage" || i >= len(as.Rhs) {
		return false
	}
	id, isID := sel.X.(*ast.Ident)
	if !isID || flagset == "" || id.Name != flagset {
		return false
	}
	switch r := as.Rhs[i].(type) {
	case *ast.FuncLit:
		return callsHelptext(r, pages)
	case *ast.Ident:
		return themedFuncs[r.Name]
	}
	return false
}

var flagDefMethods = map[string]int{ // method -> index of the usage argument
	"Bool": 2, "String": 2, "Int": 2, "Int64": 2, "Uint": 2, "Uint64": 2, "Duration": 2, "Float64": 2, "Var": 2,
	"BoolVar": 3, "StringVar": 3, "IntVar": 3, "Int64Var": 3, "UintVar": 3, "Uint64Var": 3, "DurationVar": 3, "Float64Var": 3,
}

// scanHelpGuard checks every non-test .go file in root's source dirs (never ~/.claude's session state) for violations.
func scanHelpGuard(t *testing.T, root string) []guardFinding {
	t.Helper()
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	for _, src := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, src), func(p string, d fs.DirEntry, err error) error {
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

// guardScan accumulates the findings of one file.
type guardScan struct {
	fset          *token.FileSet
	decls         map[string]string
	themed        map[string]bool
	pages         map[string]ast.Expr
	allowFlagSets bool
	out           []guardFinding
}

func (g *guardScan) add(n ast.Node, msg string) {
	g.out = append(g.out, guardFinding{g.fset.Position(n.Pos()).String(), msg})
}

func scanFile(fset *token.FileSet, f *ast.File, decls map[string]string, themed map[string]bool, pages map[string]ast.Expr, allowFlagSets bool) []guardFinding {
	g := &guardScan{fset: fset, decls: decls, themed: themed, pages: pages, allowFlagSets: allowFlagSets}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, _ := strconv.Unquote(lit.Value); strings.Contains(s, "Usage of") {
				g.add(lit, `legacy "Usage of" string literal: use helptext.Page`)
			}
		}
		return true
	})
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			g.scanFunc(fd)
		}
	}
	return g.out
}

func (g *guardScan) scanFunc(fd *ast.FuncDecl) {
	flagsets := flagsetNames(fd.Body)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			g.checkCall(fd.Body, c, flagsets[c])
		}
		return true
	})
}

// flagsetNames maps each call expression assigned to a variable in body to that variable's name.
func flagsetNames(body *ast.BlockStmt) map[*ast.CallExpr]string {
	flagsets := map[*ast.CallExpr]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range d.Rhs {
				c, ok := rhs.(*ast.CallExpr)
				if !ok || i >= len(d.Lhs) {
					continue
				}
				if id, ok := d.Lhs[i].(*ast.Ident); ok {
					flagsets[c] = id.Name
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
	return flagsets
}

func (g *guardScan) checkCall(body *ast.BlockStmt, c *ast.CallExpr, flagset string) {
	if isSel(c.Fun, "flag", "NewFlagSet") && !g.allowFlagSets && !usageAssigned(body, flagset, g.themed, g.pages) {
		g.add(c, "flag.NewFlagSet without shared help: use helptext.NewFlagSet or assign Usage calling helptext.Page.Fprint/helptext.Fprint")
	}
	g.checkBacktick(c)
	if isSel(c.Fun, "fmt", "Fprint") || isSel(c.Fun, "fmt", "Fprintln") || isSel(c.Fun, "fmt", "Fprintf") {
		if len(c.Args) >= 2 && isSel(c.Args[0], "os", "Stderr") && usageText(c.Args[1], g.decls) {
			g.add(c, `plain fmt write of a "usage:" string to os.Stderr: use helptext.Fprint`)
		}
	}
}

// checkBacktick flags a flag-definition call whose literal usage string contains a backtick.
func (g *guardScan) checkBacktick(c *ast.CallExpr) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	idx, isDef := flagDefMethods[sel.Sel.Name]
	if !isDef || len(c.Args) <= idx {
		return
	}
	lit, isLit := c.Args[idx].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		return
	}
	if s, _ := strconv.Unquote(lit.Value); strings.Contains(s, "`") {
		g.add(c, "flag usage string contains a backtick (flag renames the value type after it): "+s)
	}
}

func usageText(e ast.Expr, decls map[string]string) bool {
	if id, ok := e.(*ast.Ident); ok {
		return strings.HasPrefix(decls[id.Name], "usage:")
	}
	return strings.HasPrefix(firstString(e), "usage:")
}

// TestHelpGuard enforces shared help renderers and rejects legacy headers and raw usage writes.
func TestHelpGuard(t *testing.T) {
	for _, f := range scanHelpGuard(t, "../../..") {
		t.Errorf("%s: %s", f.pos, f.msg)
	}
}

// helpGuardFixtures are source snippets and the number of guard findings each must produce.
var helpGuardFixtures = []struct {
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
}

func TestHelpGuardFixtures(t *testing.T) {
	for _, tc := range helpGuardFixtures {
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

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
