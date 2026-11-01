package memory

import (
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/vault"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
)

// canonTemp resolves symlinks so fixture paths match what the package resolves at runtime.
func canonTemp(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// rawGit runs git against dir bypassing the package's sanitized Repo.run, for building/inspecting fixtures.
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(proc.GitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolatedConfig isolates CLAUDE_CONFIG_DIR, HOME and XDG_CONFIG_HOME under fresh temp dirs.
func isolatedConfig(t *testing.T) (cfg, home string) {
	t.Helper()
	cfg = canonTemp(t, t.TempDir())
	home = canonTemp(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return cfg, home
}

// initStore creates the unified store's git repo; callers must already hold an isolated CLAUDE_CONFIG_DIR.
func initStore(t *testing.T) {
	t.Helper()
	if err := bank.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// fixtureGit runs git on dir minus inherited GIT_* (GIT_DIR beats -C), user/system config, hooks and signing.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(proc.GitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// gitRepo makes a real disposable repository with one commit at <parent>/<name>, copied from a once-built template.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := gitRepoTemplate(t)
	if out, err := exec.Command("cp", "-a", filepath.Join(tmpl, ".git"), root).CombinedOutput(); err != nil {
		t.Fatalf("copy template repo: %v %s", err, out)
	}
	return root
}

var (
	gitTmplOnce sync.Once
	gitTmplDir  string
)

func gitRepoTemplate(t *testing.T) string {
	t.Helper()
	gitTmplOnce.Do(func() {
		dir, err := os.MkdirTemp("", "memory-gittmpl-")
		if err != nil {
			t.Fatal(err)
		}
		gitTmplDir = dir
		fixtureGit(t, dir, "init", "-q", "-b", "main")
		fixtureGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	})
	if gitTmplDir == "" {
		t.Fatal("git template repo unavailable")
	}
	return gitTmplDir
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	if gitTmplDir != "" {
		os.RemoveAll(gitTmplDir)
	}
	os.Exit(code)
}

func vaultGet(t *testing.T, key string) (string, bool) {
	t.Helper()
	var raw string
	var ok bool
	if err := vault.View(func(tx *vault.Tx) error { raw, ok = tx.Get(vault.MemoryOrigins, key); return nil }); err != nil {
		t.Fatal(err)
	}
	return raw, ok
}

// isolatedConfig1 is isolatedConfig for tests that never need the home path.
func isolatedConfig1(t *testing.T) string {
	cfg, _ := isolatedConfig(t)
	return cfg
}
