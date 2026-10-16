package vscode

import (
	"encoding/json"
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSyncMergesJSONCAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "template.json")
	dest := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(tmpl, []byte(`{
  "chatgpt.cliExecutable": "__CLAUDE_BIN__/codex",
  "terminal.integrated.copyOnSelection": true,
  "nested": {"template": true}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := `{
  // local setting must survive
  "local.only": true,
  "terminal.integrated.copyOnSelection": false,
  "nested": {"local": 1,},
}`
	if err := os.WriteFile(dest, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", true)
	if err != nil || !result.Changed || result.Backup != "" {
		t.Fatalf("check = %+v err %v", result, err)
	}
	if body, _ := os.ReadFile(dest); string(body) != original {
		t.Fatal("--check rewrote Machine settings")
	}

	result, err = Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err != nil || !result.Changed || result.Backup == "" {
		t.Fatalf("sync = %+v err %v", result, err)
	}
	body, _ := os.ReadFile(dest)
	for _, want := range []string{
		`"local.only": true`, `"template": true`, `"local": 1`,
		`/opt/claude/bin/codex`, `"terminal.integrated.copyOnSelection": true`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("merged settings missing %q:\n%s", want, body)
		}
	}
	if saved, _ := os.ReadFile(result.Backup); string(saved) != original {
		t.Fatal("backup did not preserve the original JSONC")
	}

	// A second sync against the now-merged file has nothing left to change.
	result, err = Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err != nil || result.Changed {
		t.Fatalf("idempotent re-sync = %+v err %v", result, err)
	}
}

func vscodeRaceFixture(t *testing.T, live string) (tmpl, dest string) {
	t.Helper()
	dir := t.TempDir()
	tmpl = filepath.Join(dir, "template.json")
	dest = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(tmpl, []byte(`{"terminal.integrated.copyOnSelection": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	return tmpl, dest
}

// Guards the read-modify-write window: VS Code does not honour our lock, so a save landing between
// our read and our write must abort the merge — never be overwritten, never be absent from the backup.
func TestSyncRefusesAStaleMerge(t *testing.T) {
	const original = `{"local.only": true}`
	const concurrent = `{"local.only": true, "saved.by.vscode": 42}`
	tmpl, dest := vscodeRaceFixture(t, original)

	BeforeWrite = func(string) {
		if err := os.WriteFile(dest, []byte(concurrent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { BeforeWrite = nil })

	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err == nil {
		t.Errorf("stale merge was accepted: %+v", result)
	}
	if result.Changed {
		t.Error("stale merge reported changed")
	}
	if body, _ := os.ReadFile(dest); string(body) != concurrent {
		t.Errorf("concurrent save was overwritten:\n got %s\nwant %s", body, concurrent)
	}
	if result.Backup != "" {
		t.Errorf("backup %s written for a merge that must not proceed", result.Backup)
	}
}

// A write serializes on a lock in harness state (never beside VS Code's file); --check creates nothing.
func TestSyncCheckCreatesNoLockFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	tmpl, dest := vscodeRaceFixture(t, `{"local.only": true}`)
	lock := filepath.Join(paths.StateDir(), "vscode.lock")
	if _, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("--check created the lock (stat err %v)", err)
	}
	if _, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("write path did not take the state lock: %v", err)
	}
	if _, err := os.Stat(dest + ".lock"); !os.IsNotExist(err) {
		t.Errorf("a lock was written beside VS Code's settings file")
	}
}

func TestStripJSONCHandlesCommentsStringsAndTrailingCommas(t *testing.T) {
	in := `{
  // a line comment
  "a": "value with // not a comment and /* not a block */",
  "b": [1, 2, 3,],
  /* a block
     comment */
  "c": {"nested": true,},
}`
	var out map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(in)), &out); err != nil {
		t.Fatalf("stripped JSONC did not parse: %v\n%s", err, stripJSONC([]byte(in)))
	}
	if out["a"] != "value with // not a comment and /* not a block */" {
		t.Errorf("string content was mangled: %v", out["a"])
	}
	if out["b"].([]any) == nil || len(out["b"].([]any)) != 3 {
		t.Errorf("array trailing comma not stripped cleanly: %v", out["b"])
	}
	nested, ok := out["c"].(map[string]any)
	if !ok || nested["nested"] != true {
		t.Errorf("object trailing comma not stripped cleanly: %v", out["c"])
	}
}

func TestDefaultDestIsPlatformSpecific(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based home dir override does not apply on windows")
	}

	dest, err := DefaultDest()
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		want := filepath.Join(home, "Library", "Application Support", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest = %s, want %s", dest, want)
		}
	default:
		// No vscode-server dir yet: falls back to the desktop (User scope) path.
		want := filepath.Join(home, ".config", "Code", "User", "settings.json")
		if dest != want {
			t.Errorf("dest (no vscode-server) = %s, want %s", dest, want)
		}
		if err := os.MkdirAll(filepath.Join(home, ".vscode-server"), 0o755); err != nil {
			t.Fatal(err)
		}
		dest, err = DefaultDest()
		if err != nil {
			t.Fatal(err)
		}
		want = filepath.Join(home, ".vscode-server", "data", "Machine", "settings.json")
		if dest != want {
			t.Errorf("dest (with vscode-server) = %s, want %s", dest, want)
		}
	}
}
