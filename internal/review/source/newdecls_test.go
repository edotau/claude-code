package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
)

// advisoryRepo commits base, then stages head (path -> content) and returns the repo root.
func advisoryRepo(t *testing.T, base, head map[string]string) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Env = proc.GitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(files map[string]string) {
		for p, body := range files {
			if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("init", "-q")
	write(base)
	run("add", ".")
	run("commit", "-q", "-m", "base")
	write(head)
	run("add", ".")
	return root
}

func TestGoAdvisoryNewTestsAndHelpers(t *testing.T) {
	root := advisoryRepo(t,
		map[string]string{"a.go": "package p\n\nfunc keep() {}\n"},
		map[string]string{
			"a.go":      "package p\n\nfunc keep() {}\n\nfunc zero() {}\n\nfunc two() {}\n\nfunc use() { two(); two() }\n",
			"a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n",
		})
	got := GoAdvisory(root, proc.GitEnv(), "HEAD", true, []GoChange{{Path: "a.go"}, {Path: "a_test.go"}}).New
	want := []string{
		"a.go:5: new helper zero (0 callers)",
		"a.go:7: new helper two (2 callers)",
		"a.go:9: new helper use (0 callers)",
		"a_test.go:5: new test TestNew",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}
