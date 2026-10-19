package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

func aliasGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// aliasFixture: isolated config with a cut-over store and two checkouts, each with its own store.
func aliasFixture(t *testing.T) (a, b, ka, kb string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Join(cfg, "state", "harness", "migrate"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(cfg, "state", "harness", "migrate", "20260101-000000.journal.json")
	if err := os.WriteFile(j, []byte(`{"phase":"complete","manifest":"/m/done.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := memory.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range []string{"alpha", "beta"} {
		r := filepath.Join(parent, n)
		os.MkdirAll(r, 0o755)
		aliasGit(t, r, "init", "-q", "-b", "main")
		aliasGit(t, r, "commit", "-q", "--allow-empty", "-m", "init")
	}
	a, b = filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	da, err := memory.EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := memory.EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "topic.md"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a, b, filepath.Base(da), filepath.Base(db)
}

func TestMemoryAliasUsageErrors(t *testing.T) {
	a, _, ka, _ := aliasFixture(t)
	for name, args := range map[string][]string{
		"no flags":    {},
		"no into":     {"--repo", a},
		"no repo":     {"--into", ka},
		"extra arg":   {"--repo", a, "--into", ka, "stray"},
		"unknown key": {"--repo", a, "--into", "nope"},
		"self":        {"--repo", a, "--into", ka},
		"bad flag":    {"--bogus"},
	} {
		if code, _, _ := captureOutput(t, func() int { return cmdMemoryAlias(args) }); code == 0 {
			t.Errorf("%s: exit 0, want non-zero", name)
		}
	}
}

func TestMemoryAliasDryRunThenApply(t *testing.T) {
	_, b, ka, kb := aliasFixture(t)
	db, _ := memory.RepoDir(b)

	code, out, _ := captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, kb) {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("dry run removed the source store: %v", err)
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) })
	if code != 0 || !strings.HasPrefix(out, "applied:") {
		t.Fatalf("apply: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("source store survived apply: %v", err)
	}
	if got, _ := memory.RepoDir(b); filepath.Base(got) != ka {
		t.Errorf("RepoDir(beta) = %s, want key %s", got, ka)
	}
	if code, _, _ = captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) }); code == 0 {
		t.Errorf("repeat apply exited 0, want non-zero")
	}
}

func TestMemoryAliasDispatchedAndCompleted(t *testing.T) {
	if got := completionCandidates([]string{"memory", "al"}); !slices.Equal(got, []string{"alias"}) {
		t.Errorf("memory al* = %v, want [alias]", got)
	}
	got := completionCandidates([]string{"memory", "alias", "--"})
	for _, want := range []string{"--repo", "--into", "--dry-run"} {
		if !slices.Contains(got, want) {
			t.Errorf("memory alias flags missing %q: %v", want, got)
		}
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemory([]string{"alias"}) }); code == 0 {
		t.Errorf("cmdMemory alias without flags exited 0")
	}
}
