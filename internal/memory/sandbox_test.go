package memory

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
)

func TestSandboxPreservesIdentityAndSource(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "checkout")
	dir, err := Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Init(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(HomeBankDir(), "topic.md"), "home topic")
	sandboxExtras(t, dir)
	before := sandboxFiles(t, cfg)
	res, err := Sandbox(SandboxOptions{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(res.Dir); err != nil {
			t.Error(err)
		}
	})
	requireOnlyRegistryAdded(t, before, sandboxFiles(t, cfg), res.Dir)
	want := sandboxFiles(t, dir)
	for _, name := range []string{"x.lock", ".postupdate", ".git/config", "link.md"} {
		delete(want, name)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", res.Dir)
	copied := Dir(repo)
	if filepath.Base(copied) != filepath.Base(dir) {
		t.Fatalf("store key changed: %s -> %s", dir, copied)
	}
	if got := sandboxFiles(t, copied); !reflect.DeepEqual(got, want) {
		t.Fatalf("file set differs: got %v want %v", got, want)
	}
	wantFile(t, filepath.Join(HomeBankDir(), "topic.md"), "home topic")
	if got := hosttest.In(t, StoreDir(), "rev-list", "--count", "HEAD"); strings.TrimSpace(got) != "1" {
		t.Fatalf("commit count: %q", got)
	}
	sandboxVaultOnly(t)
}

// requireOnlyRegistryAdded fails unless the registry (naming dir) and its lock are the only source-config changes.
func requireOnlyRegistryAdded(t *testing.T, before, after map[string]string, dir string) {
	t.Helper()
	const reg = "state/harness/memory/sandboxes.list"
	if !strings.Contains(after[reg], dir) {
		t.Fatalf("registry %q does not name %s", after[reg], dir)
	}
	delete(after, reg)
	delete(after, reg+".lock")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source config changed")
	}
}

func TestSandboxOriginsConcurrentConfigsStayIsolated(t *testing.T) {
	vault.DisableSyncForTests()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dirs := [2]string{t.TempDir(), t.TempDir()}
	keys := [2]string{"alpha", "beta"}
	var wg sync.WaitGroup
	for i := range dirs {
		wg.Go(func() {
			if err := sandboxOrigins(dirs[i], []Origin{{Version: OriginVersion, Key: keys[i]}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i, dir := range dirs {
		err := vault.ViewAt(filepath.Join(dir, "state", "vault.db"), func(tx *vault.Tx) error {
			if got := tx.Names(vault.MemoryOrigins); !reflect.DeepEqual(got, []string{keys[i]}) {
				t.Errorf("vault %d origins = %v, want [%s]", i, got, keys[i])
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A messy registry (blank, duplicate, trailing slash, CRLF, relative, gone) keeps only the real absolute dirs.
func TestUpdateSandboxRegistryTidiesMessyFile(t *testing.T) {
	tempConfig(t)
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	write(t, SandboxRegistry(), "\n"+a+"\n\n"+a+"\n"+b+"/\n.\n"+c+"\r\n"+gone+"\n")
	live, err := UpdateSandboxRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range live {
		got[filepath.Clean(d)] = true
	}
	if want := map[string]bool{a: true, b: true, c: true}; len(live) != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("live = %q, want one entry each of %q", live, []string{a, b, c})
	}
	data, err := os.ReadFile(SandboxRegistry())
	if err != nil || string(data) != strings.Join(live, "\n")+"\n" {
		t.Errorf("registry rewritten as %q (%v), want the live entries one per line, no blanks", data, err)
	}
}

func sandboxFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			files[rel] = "symlink:" + target
			return err
		}
		data, err := os.ReadFile(p)
		files[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sandboxVaultOnly(t *testing.T) {
	t.Helper()
	err := vault.View(func(tx *vault.Tx) error {
		for _, bucket := range []string{vault.Secrets, vault.Pins, vault.Env, vault.Overlay, vault.SettingsBackups} {
			if len(tx.All(bucket)) != 0 {
				t.Errorf("sandbox contains %s rows", bucket)
			}
		}
		if len(tx.All(vault.MemoryOrigins)) != 1 {
			t.Error("missing origin metadata")
		}
		if _, ok := tx.Get(vault.Meta, "kid"); ok {
			t.Error("sandbox has a master key id")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSandboxEmptyAndUnknownRepo(t *testing.T) {
	tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "checkout")
	dir, err := Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "topic.md"), "do not copy")
	if _, err := Sandbox(SandboxOptions{Repo: "unknown-sandbox-store"}); err == nil {
		t.Fatal("unknown repo accepted")
	}
	res, err := Sandbox(SandboxOptions{Repo: repo, Empty: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(res.Dir); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", res.Dir)
	if got := sandboxFiles(t, Dir(repo)); len(got) != len(templates()) {
		t.Fatalf("empty bank: %v", got)
	}
	for name, body := range templates() {
		wantFile(t, filepath.Join(Dir(repo), name), body)
	}
}

func sandboxExtras(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"MEMORY.md", "archive/a.md", "reports/r.md", "handoffs/h.md", "knowledge/k.md", "x.lock", ".postupdate", ".git/config"} {
		write(t, filepath.Join(dir, name), name)
	}
	if err := os.Symlink("MEMORY.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
}
