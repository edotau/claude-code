package schemas

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/nonet" // test binaries refuse real DNS
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

type rec struct {
	Schema int    `json:"schema"`
	Name   string `json:"name"`
}

func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func TestDecodeV0ThroughSameShapeReportsFromZero(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-v0", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	from, err := Decode(f, []byte(`{"name":"a"}`), &got)
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || got.Name != "a" {
		t.Fatalf("from=%d got=%+v, want from=0 name=a", from, got)
	}
}

func TestDecodeNewerRecordReturnsErrNewer(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-newer", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	if _, err := Decode(f, []byte(`{"schema":2,"name":"a"}`), &got); !errors.Is(err, ErrNewer) {
		t.Fatalf("err=%v, want ErrNewer", err)
	}
}

func TestDecodeRejectsNegativeSchemaBeforeMigration(t *testing.T) {
	calls := 0
	f := Format{Name: "negative-schema", Version: 1, Migrate: func(_ int, _ []byte) ([]byte, error) {
		calls++
		return nil, errors.New("unexpected migration")
	}}
	var got rec
	_, err := Decode(f, []byte(`{"schema":-2147483648}`), &got)
	if calls != 0 {
		t.Fatalf("negative schema invoked migration %d times, want 0", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "negative schema version -2147483648") {
		t.Fatalf("Decode error = %v, want negative schema version rejection", err)
	}
}

func TestDecodeOlderWithoutMigrateReturnsErrNoMigration(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-nomig", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"})
	var got rec
	if _, err := Decode(f, []byte(`{"name":"a"}`), &got); !errors.Is(err, errNoMigration) {
		t.Fatalf("err=%v, want ErrNoMigration", err)
	}
}

// reg registers f for one test and drops the row at cleanup, so -count>1 re-registers cleanly.
func reg(t *testing.T, f Format) Format {
	t.Helper()
	t.Cleanup(func() { mu.Lock(); delete(rows, f.Name); mu.Unlock() })
	return Register(f)
}

func TestRegisterPanicsOnDuplicateName(t *testing.T) {
	f := Format{Name: "t-dup", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"}
	reg(t, f)
	defer func() {
		if recover() == nil {
			t.Fatal("second Register of the same Name did not panic")
		}
	}()
	Register(f)
}

// TestDecodeNullIsErrorNotPanic: a `null` document is rejected, never a nil-map panic.
func TestDecodeNullIsErrorNotPanic(t *testing.T) {
	f := Format{Name: "null-doc", Version: 1, Migrate: SameShape}
	var got map[string]any
	if _, err := Decode(f, []byte("null"), &got); err == nil {
		t.Fatal("Decode(null) returned nil error")
	}
	if _, err := SameShape(0, []byte("null")); err == nil {
		t.Fatal("SameShape(0, null) returned nil error")
	}
}

var writeCalls = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.CreateTemp": true, "os.OpenFile": true,
	"paths.AtomicWrite": true, "paths.AtomicWriteNoSync": true, "paths.AppendLine": true, "paths.WriteIfChanged": true, "paths.WriteJSON": true, "paths.Backup": true,
	"paths.WriteIfUnchanged": true,
}

func scanWriters(t *testing.T) map[string]bool {
	t.Helper()
	root := ".."
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(filepath.Join("internal", strings.TrimPrefix(filepath.ToSlash(p), "../")))
		if d.IsDir() {
			if d.Name() == "testdata" || rel == "internal/paths" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		return scanWriterFile(p, rel, found)
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func scanWriterFile(path, rel string, found map[string]bool) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return err
	}
	scanWriterDeclarations(f, rel, found)
	return nil
}

func scanWriterDeclarations(f *ast.File, rel string, found map[string]bool) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if functionWrites(fn) {
			found[rel+":"+fn.Name.Name] = true
		}
	}
}

func functionWrites(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := callName(c); writeCalls[name] && (name != "os.OpenFile" || createsFile(c)) {
			found = true
		}
		return true
	})
	return found
}

func callName(c *ast.CallExpr) string {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name + "." + sel.Sel.Name
}

func createsFile(c *ast.CallExpr) bool {
	if len(c.Args) < 2 {
		return false
	}
	hit := false
	ast.Inspect(c.Args[1], func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			switch s.Sel.Name {
			case "O_CREATE", "O_APPEND", "O_WRONLY":
				hit = true
			}
		}
		return true
	})
	return hit
}

// goldenNames reads the format names from the registry golden, which internal/cli checks against the linked binary.
func goldenNames(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("testdata/registry.golden")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for l := range strings.SplitSeq(string(b), "\n") {
		if name, _, ok := strings.Cut(l, " | "); ok && !strings.HasPrefix(l, "#") {
			names[name] = true
		}
	}
	return names
}

const writersDir = "testdata/writers"

// Rows are `<file.go>:<func>` TAB `<format>|unversioned: <why>`, sorted, in testdata/writers/<pkg path, - for />.txt.
func writerFile(key string) string {
	file, _, _ := strings.Cut(key, ":")
	return strings.ReplaceAll(path.Dir(file), "/", "-") + ".txt"
}

func loadWriters(dir string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	rows := map[string]string{}
	for _, f := range files {
		if err := loadWriterFile(f, rows); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func loadWriterFile(f string, rows map[string]string) error {
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	base := filepath.Base(f)
	prev := ""
	for i, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		key, val, ok := strings.Cut(l, "\t")
		switch {
		case !ok || key == "" || val == "":
			return fmt.Errorf("%s:%d: want <key><TAB><schema or unversioned: reason>", base, i+1)
		case writerFile(key) != base:
			return fmt.Errorf("%s:%d: row %s belongs in %s", base, i+1, key, writerFile(key))
		case key <= prev:
			return fmt.Errorf("%s:%d: row %s is duplicate or unsorted", base, i+1, key)
		}
		if _, dup := rows[key]; dup {
			return fmt.Errorf("%s:%d: duplicate row %s", base, i+1, key)
		}
		prev, rows[key] = key, val
	}
	return nil
}

func TestWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("pre-ship tier: whole-repo AST walk")
	}
	writers, err := loadWriters(writersDir)
	if err != nil {
		t.Fatal(err)
	}
	found := scanWriters(t)
	names := goldenNames(t)
	for _, issue := range registeredWriterIssues(found, writers, names) {
		t.Error(issue)
	}
	for _, issue := range staleWriterIssues(found, writers) {
		t.Error(issue)
	}
}

func registeredWriterIssues(found map[string]bool, registry map[string]string, names map[string]bool) []string {
	var issues []string
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := registry[k]
		if !ok {
			issues = append(issues, fmt.Sprintf("%s writes a file: add this row to internal/schemas/%s/%s", k, writersDir, writerFile(k)))
			continue
		}
		if !names[v] && !strings.HasPrefix(v, "unversioned: ") {
			issues = append(issues, fmt.Sprintf("%s maps to %q: not a registered format name nor unversioned: <reason> (%s)", k, v, writerFile(k)))
		}
	}
	return issues
}

func staleWriterIssues(found map[string]bool, registry map[string]string) []string {
	var issues []string
	for k := range registry {
		if !found[k] {
			issues = append(issues, fmt.Sprintf("internal/schemas/%s/%s: row %s no longer has a write site: remove it", writersDir, writerFile(k), k))
		}
	}
	sort.Strings(issues)
	return issues
}

func TestWriterRegistryRejectsUnregisteredAndStale(t *testing.T) {
	const source = `package fixture
	func write() { _ = os.WriteFile("file", nil, 0600) }
	func read() { _, _ = os.OpenFile("file", os.O_RDONLY, 0600) }
	func appendFile() { _, _ = os.OpenFile("file", os.O_APPEND, 0600) }`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	scanWriterDeclarations(f, "fixture.go", found)
	t.Run("unregistered", func(t *testing.T) {
		issues := registeredWriterIssues(found, map[string]string{}, map[string]bool{})
		want := []string{
			"fixture.go:appendFile writes a file: add this row to internal/schemas/testdata/writers/..txt",
			"fixture.go:write writes a file: add this row to internal/schemas/testdata/writers/..txt",
		}
		if strings.Join(issues, "\n") != strings.Join(want, "\n") {
			t.Fatalf("unregistered writer issues = %v, want %v", issues, want)
		}
	})
	t.Run("stale", func(t *testing.T) {
		registry := map[string]string{"fixture.go:read": "unversioned: foreign: fixture"}
		issues := staleWriterIssues(found, registry)
		if len(issues) != 1 || issues[0] != "internal/schemas/testdata/writers/..txt: row fixture.go:read no longer has a write site: remove it" {
			t.Fatalf("stale writer issues = %v", issues)
		}
	})
}
