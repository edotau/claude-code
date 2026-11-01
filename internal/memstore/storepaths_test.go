package memstore

import (
	"os"
	"path/filepath"
	"testing"
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestStorePathsAndInsideStore(t *testing.T) {
	cfg := tempConfig(t)
	// No origin remote here, so the harness key falls back to the config dir's own basename, not ".claude".
	if StoreDir() != filepath.Join(cfg, "memory") || HarnessStoreDir() != filepath.Join(cfg, "memory", filepath.Base(cfg)) {
		t.Fatal("store paths moved")
	}
	if JournalDir() != filepath.Join(StateDir(), "journal") || KnowledgeDir("/x") != "/x/knowledge" {
		t.Fatal("journal/knowledge paths moved")
	}
	if ArchiveDir("/x") != "/x/archive" || HandoffsDir("/x") != "/x/handoffs" {
		t.Fatal("archive/handoffs paths moved")
	}
	cases := map[string]struct {
		rel string
		ok  bool
	}{
		filepath.Join(StoreDir(), "repo", "MEMORY.md"): {"repo/MEMORY.md", true},
		filepath.Join(StoreDir(), ".git", "config"):    {"", false},
		filepath.Join(StoreDir(), ".gitx"):             {".gitx", true},
		filepath.Join(cfg, "memory-other", "x.md"):     {"", false},
		filepath.Join(cfg, "docs", "x.md"):             {"", false},
		StoreDir():                                     {".", true},
	}
	for p, want := range cases {
		if rel, ok := InsideStore(p); rel != want.rel || ok != want.ok {
			t.Errorf("InsideStore(%q) = %q, %v; want %q, %v", p, rel, ok, want.rel, want.ok)
		}
	}
	// A symlink escaping the store is outside it even though its lexical path is inside.
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(StoreDir(), "escape.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := InsideStore(filepath.Join(StoreDir(), "escape.md")); ok {
		t.Error("InsideStore accepted a symlink escaping the store")
	}
}
