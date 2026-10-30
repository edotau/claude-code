package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cleanGo = "package fx\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

// longGo is cleanGo plus a 2-line comment block and a function over the medium 50-line limit.
func longGo() string {
	var b strings.Builder
	b.WriteString(cleanGo)
	b.WriteString("\n// first prose line\n// second prose line\nfunc Long() int {\n\tx := 0\n")
	for i := range 60 {
		fmt.Fprintf(&b, "\tx += %d\n", i)
	}
	b.WriteString("\treturn x\n}\n")
	return b.String()
}

func gitFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	writeFx(t, dir, "fx.go", cleanGo)
	writeFx(t, dir, "notes.txt", "a\n")
	for _, a := range [][]string{{"add", "."}, {"commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	t.Chdir(dir)
	return dir
}

func writeFx(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runSimplifyJSON(t *testing.T, args ...string) simplifyReport {
	t.Helper()
	var out, errb bytes.Buffer
	if code := RunSimplify(append(args, "--json"), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	var r simplifyReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out.String())
	}
	return r
}

func hasSmell(f simplifyFile, typ string) bool {
	for _, s := range f.Quality.Smells {
		if s.Type == typ {
			return true
		}
	}
	return false
}

func TestRunSimplifyChangedFiles(t *testing.T) {
	dir := gitFixture(t)
	writeFx(t, dir, "fx.go", longGo())
	writeFx(t, dir, "notes.txt", "changed\n")

	r := runSimplifyJSON(t, "--base", "HEAD")
	if len(r.Files) != 1 || r.Files[0].File != "fx.go" {
		t.Fatalf("files = %+v, want only fx.go (notes.txt is not source)", r.Files)
	}
	f := r.Files[0]
	if !hasSmell(f, "long_comment") {
		t.Errorf("smells = %+v, want a long_comment", f.Quality.Smells)
	}
	if len(f.Complexity) == 0 {
		t.Errorf("want a complexity finding for the %d-line function", 60)
	}
	if r.Summary.Files != 1 || r.Summary.Complexity != len(f.Complexity) || r.Summary.Quality != len(f.Quality.Smells) {
		t.Errorf("summary %+v disagrees with the file entry", r.Summary)
	}

	var out, errb bytes.Buffer
	if code := RunSimplify([]string{"--base", "HEAD"}, &out, &errb); code != 0 {
		t.Fatalf("text exit %d: %s", code, errb.String())
	}
	want := fmt.Sprintf("Findings: %d (complexity %d, quality %d)", len(f.Complexity)+len(f.Quality.Smells), len(f.Complexity), len(f.Quality.Smells))
	for _, s := range []string{"Simplify Check — 1 files, threshold medium", "fx.go", "long_comment", want} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("text output missing %q:\n%s", s, out.String())
		}
	}
}

func TestRunSimplifyUntrackedAndExplicitPath(t *testing.T) {
	dir := gitFixture(t)
	writeFx(t, dir, "fresh.go", longGo())

	r := runSimplifyJSON(t, "--base", "HEAD")
	if len(r.Files) != 1 || r.Files[0].File != "fresh.go" {
		t.Fatalf("untracked new file not found: %+v", r.Files)
	}
	// An explicit path ignores git state, so the unchanged committed file is checked too.
	r = runSimplifyJSON(t, "fx.go")
	if len(r.Files) != 1 || r.Files[0].File != "fx.go" || hasSmell(r.Files[0], "long_comment") {
		t.Fatalf("explicit path run = %+v, want clean fx.go only", r.Files)
	}
}

func TestRunSimplifyNoSourceChanges(t *testing.T) {
	dir := gitFixture(t)
	writeFx(t, dir, "notes.txt", "changed\n")

	var out, errb bytes.Buffer
	if code := RunSimplify([]string{"--base", "HEAD"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "No changed source files." {
		t.Errorf("stdout = %q", out.String())
	}

	out.Reset()
	if code := RunSimplify([]string{"--base", "HEAD", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	if got := out.String(); !strings.Contains(got, `"files": []`) && !strings.Contains(got, `"files":[]`) {
		t.Errorf("json files must be [] not null: %s", got)
	}
}

func TestRunSimplifyBadThreshold(t *testing.T) {
	var out, errb bytes.Buffer
	if code := RunSimplify([]string{"--threshold", "nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "invalid choice") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestSimplicityFindings(t *testing.T) {
	noisy := cleanGo + "\n// old prose line one\n// old prose line two\nfunc Old() int { return 1 }\n"
	shifted := "// header\n// header2\n\n" + strings.Replace(noisy, "return a + b", "return a + b + 0", 1)
	cases := []struct {
		name, path, before, after string
		want                      []string // substrings, one per expected finding
	}{
		{"clean to comment block", "fx.go", cleanGo, cleanGo + "\n// first\n// second\nfunc Z() {}\n", []string{"long_comment"}},
		{"noisy plus unrelated shifting edit", "fx.go", noisy, shifted, nil},
		{"noisy plus one new block", "fx.go", noisy, shifted + "\n// new one\n// new two\nfunc N() {}\n", []string{"long_comment"}},
		{"unsupported ext", "notes.md", cleanGo, cleanGo + "\n// a\n// b\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SimplicityFindings(c.path, c.before, c.after)
			if len(got) != len(c.want) {
				t.Fatalf("got %d findings %q, want %d", len(got), got, len(c.want))
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("finding %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
	if got := SimplicityFindings("fx.go", longGo(), longGo()); len(got) != 0 {
		t.Errorf("identical before/after reported %q", got)
	}
}
