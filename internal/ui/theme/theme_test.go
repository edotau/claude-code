package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// Apply merges a theme file over a tool config (keeping foreign keys), backs the original up, and is
// a no-op when nothing differs; dry-run reports the change without writing.
func TestApplyMergesKeepsForeignKeysAndIsIdempotent(t *testing.T) {
	dir, home, dest := applyFixture(t)

	assertDryRunKeepsFile(t, dir, home, dest)
	out, err := Apply(dir, home, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	o := find(out, "copilot")
	b, _ := os.ReadFile(dest)
	if !o.Changed || o.Backup == "" || !appliedMergeKept(string(b)) {
		t.Fatalf("apply: %+v\n%s", o, b)
	}
	out, _ = Apply(dir, home, nil, false)
	if o := find(out, "copilot"); o.Changed {
		t.Error("second apply must be a no-op")
	}
	if o := find(out, "gemini"); o.Skipped == "" {
		t.Errorf("missing theme file must be skipped, got %+v", o)
	}
}

// --only touches just the named target (a prefix picks its "<name>/…" rows) and rejects an unknown name.
func TestApplyOnlyLimitsTargets(t *testing.T) {
	dir, home, dest := applyFixture(t)
	before, _ := os.ReadFile(dest)
	out, err := Apply(dir, home, []string{"opencode-themes"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "opencode-themes/gemini-plum" {
		t.Fatalf("only opencode-themes: %+v", out)
	}
	if after, _ := os.ReadFile(dest); !bytes.Equal(before, after) {
		t.Error("--only opencode-themes rewrote the copilot config")
	}
	if _, err := Apply(dir, home, []string{"nope"}, false); err == nil || !strings.Contains(err.Error(), "copilot") {
		t.Errorf("unknown target must error listing valid names, got %v", err)
	}
}

// assertDryRunKeepsFile requires a dry-run Apply to report the copilot change without backing up or writing dest.
func assertDryRunKeepsFile(t *testing.T, dir, home, dest string) {
	t.Helper()
	out, err := Apply(dir, home, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if o := find(out, "copilot"); !o.Changed || o.Backup != "" {
		t.Fatalf("dry-run: %+v", o)
	}
	if b, _ := os.ReadFile(dest); !strings.Contains(string(b), "github") {
		t.Fatal("dry-run must not write")
	}
}

// applyFixture builds a HOME with a copilot theme file and a settings.json carrying a foreign key.
func applyFixture(t *testing.T) (dir, home, dest string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	dir = filepath.Join(home, "theme")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "copilot.json"), []byte(`{"theme":"default"}`), 0o644)
	dest = filepath.Join(home, ".copilot", "settings.json")
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.WriteFile(dest, []byte("{\n  // keep me\n  \"theme\": \"github\", \"model\": \"x\"\n}\n"), 0o600)
	return dir, home, dest
}

// appliedMergeKept reports whether the merged settings carry the new theme and the foreign model key.
func appliedMergeKept(b string) bool {
	return strings.Contains(b, `"theme": "default"`) && strings.Contains(b, `"model": "x"`)
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

func TestApplyReportsMergeErrorForEachFormat(t *testing.T) {
	for _, tc := range []struct{ src, body, dest string }{
		{"gemini.json", `{"theme":"new"}`, ".gemini/settings.json"},
		{"ghostty.conf", "theme = new\n", ".config/ghostty/config"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.src), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(home, tc.dest)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			oldBeforeWrite := beforeWrite
			t.Cleanup(func() { beforeWrite = oldBeforeWrite })
			beforeWrite = func(path string) { _ = os.WriteFile(path, []byte("{\"x\":1}\n"), 0o600) }

			if _, err := Apply(dir, home, nil, false); err == nil {
				t.Fatal("Apply hid the merge's concurrent-edit error")
			}
		})
	}
}

// TestSetGradientRejectsNullConfigWithoutPanicking: json.Unmarshal("null", &m) succeeds but leaves m
// nil — a bare `if err != nil` check would miss it and panic on the m["gradient"] = assignment.
// SetGradient must error out instead, and must not touch the file.
func TestSetGradientRejectsNullConfigWithoutPanicking(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "state", "statusline", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := SetGradient("ocean")
	if err == nil {
		t.Fatal("SetGradient on a null config: err = nil, want an error")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != "null" {
		t.Errorf("SetGradient on a null config changed the file: %q", raw)
	}
}

func TestMergeFileHoldsDestinationLock(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "config.json")
	initial := []byte(`{"theme":"old","model":"keep"}`)
	if err := os.WriteFile(dest, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := beforeWrite
	t.Cleanup(func() { beforeWrite = previous })
	called := false
	beforeWrite = func(path string) {
		called = true
		lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if paths.TryLock(lock) {
			t.Error("merge does not hold destination lock; a concurrent writer can overwrite the checked contents")
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}

	changed, backup, err := mergeFile(dest, map[string]any{"theme": "new"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !changed || backup == "" {
		t.Fatalf("hook called = %v, changed = %v, backup = %q", called, changed, backup)
	}
	assertMergeLockFileContents(t, backup, initial)
	assertMergeLockFileContents(t, dest, []byte("{\n  \"model\": \"keep\",\n  \"theme\": \"new\"\n}\n"))
}

func assertMergeLockFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
