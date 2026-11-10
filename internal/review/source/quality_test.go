package source

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// golden commands (captured against this exact testdata dir, python3 on PATH):
//   cd testdata/quality && python3 code_quality_checker.py <path> [--json]
// See testdata/quality/README for the full list.

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "quality", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return expandGolden(string(b))
}

// Go methods belong to their receiver type, not the nearest type declaration; only Big crosses 20.
func TestQualityGoGodClassUsesReceiver(t *testing.T) {
	var interleaved, proximity strings.Builder
	interleaved.WriteString("package fixture\ntype Big struct{}\ntype Small struct{}\n")
	proximity.WriteString("type Small struct{}\n\nfunc (s Small) One() {}\n\ntype Big struct{}\n\n")
	for i := range 21 {
		fmt.Fprintf(&interleaved, "func Free%d() {}\nfunc (b *Big) Method%d() {}\n", i, i)
		fmt.Fprintf(&proximity, "func (b *Big) M%d() {}\n", i)
	}
	proximity.WriteString("func (s *Set[T]) Add() {}\n")
	for _, tc := range []struct {
		name, src string
		methods   map[string]int
	}{
		{"free functions interleaved", interleaved.String(), map[string]int{"Big": 21, "Small": 0}},
		{"methods after an earlier type", proximity.String(), map[string]int{"Small": 1, "Big": 21}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := QualityContent("fixture.go", tc.src, "go")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, c := range report.ClassDetails {
				got[c.Name] = c.Methods
			}
			if !maps.Equal(got, tc.methods) {
				t.Errorf("methods = %v, want %v", got, tc.methods)
			}
			want := []Smell{{
				Type: "god_class", Severity: "high", Location: "Big",
				Message: "Class 'Big' has 21 methods (max: 20)",
			}}
			if !slices.Equal(report.Smells, want) {
				t.Fatalf("smells = %+v, want only Big flagged: %+v", report.Smells, want)
			}
		})
	}
}

func TestQualityGoComplexityExcludesInterveningDeclarations(t *testing.T) {
	keywords := strings.Repeat("if ", 11)
	declarations := map[string]string{
		"const":       "const text = `" + keywords + "&& || for case`\n",
		"const group": "const (\ntext = `" + keywords + "`\n)\n",
		"var group":   "var (\ntext = `" + keywords + "`\n)\n",
		"type":        "type Data struct {\nText string `" + keywords + "`\n}\n",
	}
	for name, declaration := range declarations {
		t.Run(name, func(t *testing.T) {
			src := "package fixture\nfunc Simple() {}\n" + declaration +
				"func Complex(x bool) {\n" + strings.Repeat("if x {}\n", 10) + "}\n"
			report, err := QualityContent("fixture.go", src, "go")
			if err != nil {
				t.Fatal(err)
			}
			wantFunctions := []FunctionInfo{
				{Name: "Simple", Lines: 1, Complexity: 1},
				{Name: "Complex", Parameters: 1, Lines: 12, Complexity: 11},
			}
			if !slices.Equal(report.FunctionDetails, wantFunctions) {
				t.Errorf("functions = %+v, want %+v", report.FunctionDetails, wantFunctions)
			}
			wantSmells := []Smell{{
				Type: "high_complexity", Severity: "medium", Location: "Complex",
				Message: "Function 'Complex' has complexity 11 (max: 10)",
			}}
			if !slices.Equal(report.Smells, wantSmells) {
				t.Errorf("smells = %+v, want %+v", report.Smells, wantSmells)
			}
		})
	}
}

func TestRunQualityMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"file text", []string{"paths.go"}, "paths.go.want"},
		{"file json", []string{"paths.go", "--json"}, "paths.go.want.json"},
		{"js file text", []string{"gate-loop.js"}, "gate-loop.js.want"},
		{"js file json", []string{"gate-loop.js", "--json"}, "gate-loop.js.want.json"},
		{"dir text", []string{"."}, "dir.want"},
		{"dir json", []string{".", "--json"}, "dir.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(filepath.Join("testdata", "quality"))
			var stdout, stderr bytes.Buffer
			code := RunQuality(tc.args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want, err := os.ReadFile(tc.want)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			// Python's print() adds a trailing newline; the golden capture already has it.
			expected := expandGolden(string(want))
			if stdout.String() != expected {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), expected)
			}
		})
	}
}

func TestRunQualityMissingPath(t *testing.T) {
	want := readGolden(t, "notfound.want")
	t.Chdir(filepath.Join("testdata", "quality"))
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"nonexistent.go"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.String() != want {
		t.Errorf("stderr mismatch:\ngot:  %q\nwant: %q", stderr.String(), want)
	}
}

func TestRunQualityBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"--nope", "x.go"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d for a bad flag, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a bad flag", stdout.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Errorf("stderr = %q, want the stdlib unknown-flag message", stderr.String())
	}
}

// -h after the positional path prints the usage page to stdout, exit 0, stderr empty.
func TestRunQualityHelpAfterPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"x.go", "-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "usage: claude-code review quality <path> [flags]\n") {
		t.Errorf("stdout = %q, want the review quality usage page", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on -h", stderr.String())
	}
}

// "--" before -h scans the path literally instead of printing help.
func TestRunQualityHelpAfterDoubleDashIsPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"--", "nonexistent-h-named-dir"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (missing path, not help)", code)
	}
	if strings.Contains(stdout.String(), "usage:") || strings.Contains(stderr.String(), "usage:") {
		t.Errorf("printed the usage page instead of scanning: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunQualityOutputWriteError(t *testing.T) {
	output := filepath.Join(t.TempDir(), "nonexistent", "out.json")
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{filepath.Join("testdata", "quality", "paths.go"), "--json", "--output", output}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "Error: ") || !strings.Contains(stderr.String(), output) {
		t.Errorf("stderr = %q, want write error containing %q", stderr.String(), output)
	}
}

func TestCommentSmells(t *testing.T) {
	long := "// " + strings.Repeat("x", 118)  // 121 chars
	short := "// " + strings.Repeat("x", 113) // 116 chars
	cases := []struct {
		name, lang, src string
		want            []string // "type@location"
	}{
		{"go two-line block", "go", "package a\n\n// first\n// second\nfunc f() {}\n", []string{"long_comment@line 3"}},
		{"go single line", "go", "package a\n\n// only one\nfunc f() {}\n", nil},
		{"go over 120 chars", "go", "package a\n\n" + long + "\nfunc f() {}\n", []string{"long_comment_line@line 3"}},
		{"go 116 chars ok", "go", "package a\n\n" + short + "\nfunc f() {}\n", nil},
		{"python docstring with sections", "python", "def f(a):\n    \"\"\"Do it.\n\n    Args:\n        a: the input value\n\n    Returns:\n        the result\n    \"\"\"\n    return a\n", nil},
		{"python 3-line prose docstring", "python", "def f(a):\n    \"\"\"Do it.\n    More prose here.\n    Even more prose.\n    \"\"\"\n    return a\n", []string{"long_comment@line 2"}},
		{"file header block", "go", "// header one\n// header two\npackage a\n", nil},
		{"NOTE gotcha block", "go", "package a\n\n// NOTE: careful\n// with this\nfunc f() {}\n", nil},
		{"directive plus one prose line", "go", "package a\n\n//go:generate stringer -type=T\n// one prose line\nfunc f() {}\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, s := range commentSmells(tc.src, tc.lang) {
				if s.Severity != "low" {
					t.Errorf("severity = %q, want low", s.Severity)
				}
				got = append(got, s.Type+"@"+s.Location)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("smells = %v, want %v", got, tc.want)
			}
		})
	}
}

// testdataDir is this package's testdata, resolved at init (cwd = package dir) so goldens stay machine-independent.
var testdataDir = func() string {
	abs, _ := filepath.Abs("testdata")
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}()

// expandGolden swaps the @TESTDATA@ placeholder in a golden for this checkout's testdata path.
func expandGolden(s string) string { return strings.ReplaceAll(s, "@TESTDATA@", testdataDir) }

func TestWalkSourceFilesSkipPartsBelowRootOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "build", "proj")
	for _, rel := range []string{"a.py", "dist/skip.py", "sub/b.py"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	walkSourceFiles(root, func(p string) { got = append(got, filepath.Base(p)) })
	if strings.Join(got, ",") != "a.py,b.py" {
		t.Fatalf("got %v, want [a.py b.py]: target under build/ must be walked, dist/ below it skipped", got)
	}
}

func TestWalkSourceFilesWarnsOnUnreadableDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unreadableDir(t, root)
	r, w, _ := os.Pipe()
	orig := os.Stderr
	os.Stderr = w
	var got []string
	failed := walkSourceFiles(root, func(p string) { got = append(got, filepath.Base(p)) })
	os.Stderr = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if len(got) != 1 || got[0] != "ok.py" || len(failed) != 1 || !strings.HasPrefix(failed[0], root) {
		t.Fatalf("got %v, failed %v; want [ok.py] and one unreadable path under %s", got, failed, root)
	}
	if !strings.Contains(buf.String(), "skipping unreadable path") {
		t.Fatalf("stderr %q lacks the unreadable-path warning", buf.String())
	}
}

func TestRunQualityAnalysisErrorExitsNonZero(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"empty dir": t.TempDir(), "unsupported file": file} {
		var stdout, stderr bytes.Buffer
		if code := RunQuality([]string{path, "--json"}, &stdout, &stderr); code != 1 {
			t.Errorf("%s: exit code = %d, want 1", name, code)
		}
		if !strings.Contains(stdout.String(), `"error"`) {
			t.Errorf("%s: stdout lacks error report: %q", name, stdout.String())
		}
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
