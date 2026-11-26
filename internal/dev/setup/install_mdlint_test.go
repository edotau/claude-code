package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/dev/mdlint"
)

// TestInstallMdlintConfig: a missing store skips, dry-run writes nothing, then one written and ok after.
func TestInstallMdlintConfig(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	store, dest := filepath.Join(cfg, "memory"), filepath.Join(cfg, "memory", mdlint.RulesFile)
	const row = "mdlint   memory/" + mdlint.RulesFile
	var out bytes.Buffer
	installMdlintConfig(&out, false)
	if got := out.String(); got != row+" skip  no memory store\n" {
		t.Fatalf("missing store line = %q", got)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("install must not create the memory store: stat = %v", err)
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	installMdlintConfig(&out, false)
	if got := out.String(); got != row+" skip  no parent "+mdlint.RulesFile+"\n" {
		t.Fatalf("missing parent line = %q", got)
	}
	if err := os.WriteFile(filepath.Join(cfg, mdlint.RulesFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	installMdlintConfig(&out, true)
	if got := out.String(); !strings.HasPrefix(got, row+" would write ") ||
		!strings.HasSuffix(got, " (skipped: --dry-run)\n") {
		t.Fatalf("dry-run line = %q", got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote %s", dest)
	}
	out.Reset()
	installMdlintConfig(&out, false)
	if got := out.String(); got != row+" written\n" {
		t.Fatalf("first write line = %q", got)
	}
	assertMdlintConfig(t, dest)
	out.Reset()
	installMdlintConfig(&out, false)
	if got := out.String(); got != row+" ok\n" {
		t.Fatalf("unchanged line = %q", got)
	}
}

// TestInstallMdlintConfigRewritesStale: drifted bytes are rewritten to StoreRules("memory").
func TestInstallMdlintConfigRewritesStale(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dest := filepath.Join(cfg, "memory", mdlint.RulesFile)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, mdlint.RulesFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(`{"extends":"stale"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	installMdlintConfig(&out, false)
	if got := out.String(); got != "mdlint   memory/"+mdlint.RulesFile+" written\n" {
		t.Fatalf("stale rewrite line = %q", got)
	}
	assertMdlintConfig(t, dest)
}

func assertMdlintConfig(t *testing.T, dest string) {
	t.Helper()
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(mdlint.StoreRules("memory")) {
		t.Fatalf("config = %q, want %q", data, mdlint.StoreRules("memory"))
	}
}

// TestInstallMdlintConfigSkipsSymlinkedStore: a symlinked memory dir is never written through.
func TestInstallMdlintConfigSkipsSymlinkedStore(t *testing.T) {
	cfg, elsewhere := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, mdlint.RulesFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(cfg, "memory")); err != nil {
		t.Skip(err)
	}
	var out bytes.Buffer
	installMdlintConfig(&out, false)
	if _, err := os.Stat(filepath.Join(elsewhere, mdlint.RulesFile)); !os.IsNotExist(err) {
		t.Fatalf("wrote through a symlinked store: %v (%q)", err, out.String())
	}
}
