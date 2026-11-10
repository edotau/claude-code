package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

func TestRunCodexCatalogWrapsExitErrorWithTrimmedStderr(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '  bad catalog  \\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCodexCatalog(context.Background(), bin, home, "debug", "models", "--bundled")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("expected wrapped exit status 1, got %v", err)
	}
	if got, want := err.Error(), "exit status 1: bad catalog"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

// Given a stored catalog whose key matches, Then a launch neither spawns codex nor makes a temp home.
func TestCodexCatalogPathCacheHitSkipsSpawnAndTempHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "probe-secret")
	spawns := filepath.Join(t.TempDir(), "spawns")
	bin := pathstest.FakeVendor(t, "codex", "echo run >> "+spawns+"\necho 'codex-cli 0.155.0'")
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}
	_, key, err := catalogInputs(bin, p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(paths.StateDir(), "codex")
	if err := storeCodexCatalog(dir, filepath.Join(dir, "catalog-dbx.json"), filepath.Join(dir, "catalog-dbx.key"), []byte(`{}`), key); err != nil {
		t.Fatal(err)
	}
	orig := catalogTempDir
	catalogTempDir = func() (string, error) { t.Error("temp home created on a cache hit"); return orig() }
	t.Cleanup(func() { catalogTempDir = orig })
	got, _, err := codexCatalogPath(context.Background(), p)
	if err != nil || got != filepath.Join(dir, "catalog-dbx.json") {
		t.Fatalf("path = %q, err = %v, want the cached catalog", got, err)
	}
	if raw, _ := os.ReadFile(spawns); len(raw) != 0 {
		t.Fatalf("codex spawned on a cache hit: %q", raw)
	}
}
