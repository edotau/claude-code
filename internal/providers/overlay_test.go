package providers

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteBackupPreservesCollision(t *testing.T) {
	base := filepath.Join(t.TempDir(), "providers.json.bak-20261020-120000.123456789")
	original := []byte("original backup\n")
	data := []byte("new backup\n")
	if err := os.WriteFile(base, original, 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := writeBackup(base, data)
	if err != nil {
		t.Fatal(err)
	}
	if backup != base+"-1" {
		t.Fatalf("backup = %q, want %q", backup, base+"-1")
	}
	for path, want := range map[string][]byte{base: original, backup: data} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %o, want 600", info.Mode().Perm())
	}
}

// TestSetProviderModelSeedsBuiltinEntry: a provider absent from the overlay keeps its built-in auth/route/opencodeID
// when one slot is changed (an overlay entry replaces the built-in whole, so a models-only entry would break it).
func TestSetProviderModelSeedsBuiltinEntry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderModel("github-copilot", "haiku", "gpt-x-mini"); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["github-copilot"]
	if p.Models["haiku"] != "gpt-x-mini" || p.Models["opus"] == "" || p.OpencodeID != "github-copilot" || p.Auth.Login == "" {
		t.Errorf("entry lost built-in fields: %+v", p)
	}
}
