package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
)

func writeRepoFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if _, err := proc.RunGit(context.Background(), dir, full...); err != nil {
		t.Fatal(err)
	}
}

// newDiffRepo commits pkg/p.go, pkg/p_test.go and other/o.txt in a fresh repo.
func newDiffRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoGit(t, dir, "init", "-q")
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 1 }\n")
	writeRepoFile(t, dir, "pkg/p_test.go", "package pkg\n")
	writeRepoFile(t, dir, "other/o.txt", "one\n")
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runDiff(t *testing.T, o DiffOptions) (code int, out, errOut string) {
	t.Helper()
	var so, se bytes.Buffer
	o.Out, o.Err = &so, &se
	return Diff(o), so.String(), se.String()
}

// squeeze collapses runs of spaces so assertions do not depend on column padding.
func squeeze(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

func TestDiffListsChangesAndOutsidePath(t *testing.T) {
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 2 }\n\nfunc Q() int { return 3 }\n")
	writeRepoFile(t, dir, "pkg/new_test.go", "package pkg\n\n// t\n")
	writeRepoFile(t, dir, "other/extra.md", "x\ny\n")
	repoGit(t, dir, "mv", "other/o.txt", "other/o r.txt")

	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}})
	if code != DiffExitOutside || errOut != "" {
		t.Fatalf("exit %d stderr %q, want 3 and none\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"? +2 -0 other/extra.md\n",
		"R +0 -0 other/o.txt -> other/o r.txt\n",
		"? +3 -0 pkg/new_test.go (test)\n",
		"M +3 -1 pkg/p.go\n",
		"outside --path (2):\nother/extra.md\nother/o r.txt\n",
		"agents diff: 4 files +8 -1 (prod +5 -1, test +3 -0); 2 outside --path\n",
	} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}

	code, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}, JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil || code != DiffExitOutside {
		t.Fatalf("json exit %d err %v\n%s", code, err, jsonOut)
	}
	checkJSONMatchesText(t, rep)
}

func checkJSONMatchesText(t *testing.T, rep DiffReport) {
	t.Helper()
	var paths []string
	for _, f := range rep.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"other/extra.md", "other/o r.txt", "pkg/new_test.go", "pkg/p.go"}) {
		t.Errorf("json paths = %v", paths)
	}
	if !slices.Equal(rep.Outside, []string{"other/extra.md", "other/o r.txt"}) || rep.Files[1].From != "other/o.txt" {
		t.Errorf("json outside = %v, rename from %q", rep.Outside, rep.Files[1].From)
	}
	if tot := rep.Totals; tot.Added != 8 || tot.Deleted != 1 || tot.Test.Added != 3 || tot.Prod.Added != 5 {
		t.Errorf("json totals = %+v", tot)
	}
}

func TestDiffBaseAndCleanTree(t *testing.T) {
	dir := newDiffRepo(t)
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir}); code != 0 || out != "agents diff: no changes\n" {
		t.Errorf("clean tree: exit %d out %q", code, out)
	}
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n")
	repoGit(t, dir, "commit", "-q", "-am", "second")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Base: "HEAD~1"})
	if code != 0 || !strings.Contains(squeeze(out), "M +0 -2 pkg/p.go\n") {
		t.Errorf("--base HEAD~1: exit %d\n%s", code, out)
	}
	if code, _, errOut := runDiff(t, DiffOptions{Dir: dir, Base: "--output=x"}); code != DiffExitUsage || !strings.Contains(errOut, "must not start") {
		t.Errorf("flag-shaped base: exit %d stderr %q", code, errOut)
	}
}

func TestDiffRejectsBadPath(t *testing.T) {
	dir := newDiffRepo(t)
	for path, msg := range map[string]string{"../x": "inside the repo root", "pkg/../../x": "inside the repo root",
		"/etc": "inside the repo root", t.TempDir(): "inside the repo root", "a,b": "repeat --path"} {
		code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{path}})
		if code != DiffExitUsage || out != "" || !strings.Contains(errOut, msg) {
			t.Errorf("--path %q: exit %d stdout %q stderr %q, want 2 and %q", path, code, out, errOut, msg)
		}
	}
	got, err := cleanPaths(dir, []string{filepath.Join(dir, "pkg"), "./pkg/p.go", "rules/**/*.md"})
	if err != nil || !slices.Equal(got, []string{"pkg", "pkg/p.go", "rules/**/*.md"}) {
		t.Errorf("cleanPaths = %v, %v", got, err)
	}
}

func TestInPathsPrefixAndGlob(t *testing.T) {
	sel := []string{"internal/memory", "rules/**/*.md", "internal/cli/memory.go"}
	for p, want := range map[string]bool{
		"internal/memory/a.go": true, "internal/memory2/a.go": false, "rules/a.md": true, "rules/x/y/a.md": true,
		"rules/a.txt": false, "internal/cli/memory.go": true, "internal/cli/memory_test.go": false,
	} {
		if got := inPaths(sel, p); got != want {
			t.Errorf("inPaths(%q) = %v, want %v", p, got, want)
		}
	}
}
