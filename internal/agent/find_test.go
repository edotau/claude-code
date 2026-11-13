package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func findFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runFindGit(t, root, "init", "-q")
	files := map[string]string{
		".gitignore":  "ignored.txt\n",
		"a.txt":       "Needle\nneedlework\nneedle\n",
		"sub/b.txt":   "needle\n",
		"src/code.go": "package fixture\ntype Thing struct{}\nconst Count = 1\nvar Value, Other int\nfunc Target() {}\nfunc (Thing) Method() {}\nfunc Use() { Target(); obj.Target(); TargetExtra(); generic[int](); (Target)(); var local int; _ = local }\nfunc generic[T any]() {}\n",
		"binary.txt":  "needle\x00\n",
		"sniff.txt":   strings.Repeat("a", findSniffBytes-1) + "\x00needle\n",
		"large.txt":   strings.Repeat("x", findFileLimit) + "needle",
	}
	for p, content := range files {
		writeFindFixture(t, root, p, content)
	}
	runFindGit(t, root, "add", "--", ".gitignore", "a.txt", "sub/b.txt", "src/code.go", "binary.txt", "sniff.txt", "large.txt")
	writeFindFixture(t, root, "untracked.txt", "freshtoken")
	writeFindFixture(t, root, "ignored.txt", "needle")
	return root
}

func writeFindFixture(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func runFindGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestFindModes(t *testing.T) {
	root := findFixture(t)
	cases := []struct {
		name, pattern string
		options       FindOptions
		want          string
	}{
		{"content", "needle", FindOptions{}, "a.txt:2:needlework\na.txt:3:needle\nsub/b.txt:1:needle\n"},
		{"untracked_included", "freshtoken", FindOptions{}, "untracked.txt:1:freshtoken\n"},
		{"regex", "^Need.e$", FindOptions{}, "a.txt:1:Needle\n"},
		{"ignore_case", "NEEDLE", FindOptions{IgnoreCase: true, Word: true}, "a.txt:1:Needle\na.txt:3:needle\nsub/b.txt:1:needle\n"},
		{"word", "needle", FindOptions{Word: true}, "a.txt:3:needle\nsub/b.txt:1:needle\n"},
		{"binary_name", "binary", FindOptions{Name: true}, ""},
		{"absolute_path", "needle", FindOptions{Paths: []string{filepath.Join(root, "sub")}}, "sub/b.txt:1:needle\n"},
		{"name", `src/.*\.go$`, FindOptions{Name: true}, "src/code.go\n"},
		{"func", "^Target$", FindOptions{Func: true}, "src/code.go:5:func Target() {}\n"},
		{"method", "^Method$", FindOptions{Func: true}, "src/code.go:6:func (Thing) Method() {}\n"},
		{"type", "^Thing$", FindOptions{Def: true}, "src/code.go:2:type Thing struct{}\n"},
		{"const", "^Count$", FindOptions{Def: true}, "src/code.go:3:const Count = 1\n"},
		{"var", "Other", FindOptions{Def: true}, "src/code.go:4:var Value, Other int\n"},
		{"def_func", "^Target$", FindOptions{Def: true}, "src/code.go:5:func Target() {}\n"},
		{"local_excluded", "local", FindOptions{Def: true}, ""},
		{"callers", "Target", FindOptions{Callers: true, Files: true}, "src/code.go\n"},
		{"callers_exact", "Tar.*", FindOptions{Callers: true}, ""},
		{"generic_call", "generic", FindOptions{Callers: true, Files: true}, "src/code.go\n"},
		{"files", "needle", FindOptions{Files: true}, "a.txt\nsub/b.txt\n"},
		{"prefix", "needle", FindOptions{Paths: []string{"sub"}}, "sub/b.txt:1:needle\n"},
		{"repeat_path", "needle", FindOptions{Paths: []string{"sub", "a.txt"}}, "a.txt:2:needlework\na.txt:3:needle\nsub/b.txt:1:needle\n"},
		{"glob", "needle", FindOptions{Paths: []string{"sub/*.txt"}}, "sub/b.txt:1:needle\n"},
		{"boundary", "needle", FindOptions{Paths: []string{"su"}}, ""},
		{"budget", "needle", FindOptions{Max: 1}, "a.txt:2:needlework\n... 1 more (raise --max or narrow --path)\n"},
		{"exact_budget", "needle", FindOptions{Max: 3}, "a.txt:2:needlework\na.txt:3:needle\nsub/b.txt:1:needle\n"},
		{"file_budget", "needle", FindOptions{Max: 1, Files: true}, "a.txt\n... 1 more (raise --max or narrow --path)\n"},
		{"none", "absent", FindOptions{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkFindMode(t, root, tc.pattern, tc.options, tc.want) })
	}
}

func checkFindMode(t *testing.T, root, pattern string, o FindOptions, want string) {
	t.Helper()
	o.Dir, o.Pattern = filepath.Join(root, "sub"), pattern
	if o.Max == 0 {
		o.Max = FindDefaultMax
	}
	var out, stderr bytes.Buffer
	o.Out, o.Err = &out, &stderr
	code, wantCode := Find(o), 0
	if want == "" {
		wantCode = 1
	}
	if code != wantCode || out.String() != want || stderr.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q; want %d %q", code, out.String(), stderr.String(), wantCode, want)
	}
}

func TestFindUsage(t *testing.T) {
	root := findFixture(t)
	cases := []struct {
		name    string
		options FindOptions
	}{
		{"regex", FindOptions{Pattern: "[", Max: FindDefaultMax}},
		{"zero_budget", FindOptions{}},
		{"negative_budget", FindOptions{Max: -1}},
		{"modes", FindOptions{Name: true, Func: true, Max: FindDefaultMax}},
		{"outside_path", FindOptions{Paths: []string{"../outside"}, Max: FindDefaultMax}},
		{"comma_path", FindOptions{Paths: []string{"a,b"}, Max: FindDefaultMax}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.options
			var out, stderr bytes.Buffer
			o.Dir, o.Out, o.Err = root, &out, &stderr
			if code := Find(o); code != 2 || stderr.Len() == 0 || out.Len() != 0 {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), stderr.String())
			}
		})
	}
}

func TestFindJSON(t *testing.T) {
	root := findFixture(t)
	for _, tc := range []struct {
		pattern             string
		code, matches, more int
	}{
		{"needle", 0, 1, 1}, {"absent", 1, 0, 0},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Find(FindOptions{Dir: root, Pattern: tc.pattern, Max: 1, JSON: true, Out: &out, Err: &stderr})
			var rep FindReport
			if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
				t.Fatal(err)
			}
			if code != tc.code || rep.More != tc.more || rep.Matches == nil || len(rep.Matches) != tc.matches {
				t.Fatalf("%d %+v", code, rep)
			}
		})
	}
}

func TestFindCallLines(t *testing.T) {
	root := findFixture(t)
	var out, stderr bytes.Buffer
	code := Find(FindOptions{Dir: root, Pattern: "Target", Callers: true, Max: FindDefaultMax, Out: &out, Err: &stderr})
	if code != 0 || strings.Count(out.String(), "src/code.go:7:") != 1 || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), stderr.String())
	}
}

func TestFindErrors(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Find(FindOptions{Dir: t.TempDir(), Max: FindDefaultMax, Out: &out, Err: &stderr}); code != 2 || stderr.Len() == 0 {
		t.Fatalf("code=%d err=%q", code, stderr.String())
	}
}

func TestFindSkipsUnparseableGo(t *testing.T) {
	root := findFixture(t)
	writeFindFixture(t, root, "src/code.go", "invalid Go")
	writeFindFixture(t, root, "src/ok.go", "package fixture\nfunc Target() {}\n")
	runFindGit(t, root, "add", "--", "src/ok.go")
	for _, tc := range []struct {
		pattern, out string
		code         int
	}{{"^Target$", "src/ok.go:2:func Target() {}\n", 0}, {"^Absent$", "", 1}} {
		var out, stderr bytes.Buffer
		code := Find(FindOptions{Dir: root, Pattern: tc.pattern, Func: true, Max: FindDefaultMax, Out: &out, Err: &stderr})
		if code != tc.code || out.String() != tc.out || !strings.HasPrefix(stderr.String(), "find: skipped src/code.go: ") {
			t.Fatalf("%s: code=%d out=%q err=%q", tc.pattern, code, out.String(), stderr.String())
		}
	}
}

func TestFindStopsEarly(t *testing.T) {
	root := findFixture(t)
	for _, p := range []string{"src/a.go", "src/b.go", "src/c.go", "src/d.go", "src/e.go", "src/f.go", "src/g.go", "src/h.go"} {
		writeFindFixture(t, root, p, "package fixture\nfunc Target() {}\n")
		runFindGit(t, root, "add", "--", p)
	}
	writeFindFixture(t, root, "src/z.go", "invalid Go")
	runFindGit(t, root, "add", "--", "src/z.go")
	var out, stderr bytes.Buffer
	code := Find(FindOptions{Dir: root, Pattern: "Target", Func: true, Max: 1, Out: &out, Err: &stderr})
	want := "src/a.go:2:func Target() {}\n... 1 more (raise --max or narrow --path)\n"
	if code != 0 || out.String() != want || stderr.Len() != 0 {
		t.Fatalf("%d %q %q", code, out.String(), stderr.String())
	}
}

func TestFindSkipsSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root, outside := findFixture(t), t.TempDir()
	writeFindFixture(t, root, "dir/secret.txt", "needle\n")
	runFindGit(t, root, "add", "--", "dir/secret.txt")
	writeFindFixture(t, outside, "secret.txt", "needle OUTSIDE\n")
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	Find(FindOptions{Dir: root, Pattern: "OUTSIDE", Max: FindDefaultMax, Out: &out, Err: &stderr})
	if out.Len() != 0 {
		t.Fatalf("read outside the repo: %q", out.String())
	}
}

func TestFindPhysicalLines(t *testing.T) {
	root := findFixture(t)
	writeFindFixture(t, root, "src/code.go", "package fixture\n//line other.go:9000\nfunc Target() {}\n")
	var out, stderr bytes.Buffer
	code := Find(FindOptions{Dir: root, Pattern: "Target", Func: true, Max: FindDefaultMax, Out: &out, Err: &stderr})
	if code != 0 || out.String() != "src/code.go:3:func Target() {}\n" {
		t.Fatalf("%d %q %q", code, out.String(), stderr.String())
	}
}
