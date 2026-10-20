package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBackupPreservesCollision(t *testing.T) {
	base := filepath.Join(t.TempDir(), "settings.json.bak-20261020-120000.123456789")
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

// Apply merges a theme file over a tool config (keeping foreign keys), backs the original up, and is
// a no-op when nothing differs; dry-run reports the change without writing.
func TestApplyMergesKeepsForeignKeysAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	dir := filepath.Join(home, "theme")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "copilot.json"), []byte(`{"theme":"default"}`), 0o644)
	dest := filepath.Join(home, ".copilot", "settings.json")
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.WriteFile(dest, []byte("{\n  // keep me\n  \"theme\": \"github\", \"model\": \"x\"\n}\n"), 0o600)

	out, err := Apply(dir, home, true)
	if err != nil {
		t.Fatal(err)
	}
	if o := find(out, "copilot"); !o.Changed || o.Backup != "" {
		t.Fatalf("dry-run: %+v", o)
	}
	if b, _ := os.ReadFile(dest); !strings.Contains(string(b), "github") {
		t.Fatal("dry-run must not write")
	}
	out, err = Apply(dir, home, false)
	if err != nil {
		t.Fatal(err)
	}
	o := find(out, "copilot")
	b, _ := os.ReadFile(dest)
	if !o.Changed || o.Backup == "" || !strings.Contains(string(b), `"theme": "default"`) || !strings.Contains(string(b), `"model": "x"`) {
		t.Fatalf("apply: %+v\n%s", o, b)
	}
	out, _ = Apply(dir, home, false)
	if o := find(out, "copilot"); o.Changed {
		t.Error("second apply must be a no-op")
	}
	if o := find(out, "gemini"); o.Skipped == "" {
		t.Errorf("missing theme file must be skipped, got %+v", o)
	}
}

func TestMergeFileRejectsConcurrentEdit(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "settings.json")
	initial := []byte(`{"theme":"old"}`)
	concurrent := []byte(`{"theme":"concurrent"}`)
	if err := os.WriteFile(dest, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	oldBeforeWrite := beforeWrite
	t.Cleanup(func() { beforeWrite = oldBeforeWrite })
	beforeWrite = func(path string) {
		if err := os.WriteFile(path, concurrent, 0o600); err != nil {
			t.Errorf("simulate concurrent edit: %v", err)
		}
	}

	changed, backup, err := mergeFile(dest, map[string]any{"theme": "new"}, false)
	if err == nil {
		t.Fatal("merge accepted a concurrent edit")
	}
	if changed || backup != "" {
		t.Fatalf("merge result = changed %v, backup %q; want no write or backup", changed, backup)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, concurrent) {
		t.Fatalf("destination = %q, want concurrent contents %q", got, concurrent)
	}
}

func find(out []Outcome, name string) Outcome {
	for _, o := range out {
		if o.Name == name {
			return o
		}
	}
	return Outcome{}
}
