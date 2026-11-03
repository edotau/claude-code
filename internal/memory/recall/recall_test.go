package recall

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
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

// gitRepo makes a real disposable repository with one commit at <parent>/<name>.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	return gittest.RepoAt(t, filepath.Join(parent, name))
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}

// isolatedConfig1 is isolatedConfig for tests that never need the home path.
func isolatedConfig1(t *testing.T) string {
	cfg, _ := isolatedConfig(t)
	return cfg
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
	if _, err := memstore.EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	return root
}
