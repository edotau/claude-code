package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"

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
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a real disposable repository with one commit at <parent>/<name>.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	return hosttest.RepoAt(t, filepath.Join(parent, name))
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
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

// shortBankWait shrinks BankLockWait so a contended-lock test fails fast instead of waiting the 5s default.
func shortBankWait(t *testing.T) {
	t.Helper()
	old := BankLockWait
	BankLockWait = 100 * time.Millisecond
	t.Cleanup(func() { BankLockWait = old })
}

func tempConfig(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return cfg
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repoRoot makes a git work-tree root (a .git dir is enough for the stat-first check).
func repoRoot(t *testing.T, name string) string {
	t.Helper()
	root := gitRepo(t, canonTemp(t, t.TempDir()), name)
	if _, err := EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// clearChildEnv scopes the child-env vars an o.Run update or dream sets process-wide, so later tests see them unset.
func clearChildEnv(t *testing.T) {
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
}
