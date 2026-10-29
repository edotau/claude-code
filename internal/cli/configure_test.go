package cli

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestKeyListSplitsTrimsDedupes: repeatable --drop a,b --drop c parses to [a b c]; duplicates collapse.
func TestKeyListSplitsTrimsDedupes(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"-drop", "a,b", "-drop", "c"}, []string{"a", "b", "c"}},
		{[]string{"-drop", "a,a"}, []string{"a"}},
		{[]string{"-drop", " a , ,b,"}, []string{"a", "b"}},
		{[]string{"-drop", "a", "-drop", "b,a"}, []string{"a", "b"}},
	}
	for _, c := range cases {
		var k keyList
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Var(&k, "drop", "")
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(k, c.want) {
			t.Errorf("%v -> %v, want %v", c.args, k, c.want)
		}
	}
}

// TestConfigureDropRefusesNonClaudeAgent: --agent codex --drop x exits 2 and never touches the vault.
func TestConfigureDropRefusesNonClaudeAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	code, _, _ := captureOutput(t, func() int { return cmdConfigure([]string{"--agent", "codex", "--drop", "x"}) })
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// TestConfigureHelpFitsEightyColumns: `configure -h` lines (usage + flag defaults) stay within 80 columns.
func TestConfigureHelpFitsEightyColumns(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"-h"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "--drop") || !strings.Contains(stderr, "--keep") {
		t.Fatalf("help lacks --drop/--keep:\n%s", stderr)
	}
	for l := range strings.SplitSeq(stderr, "\n") {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("help line is %d cols: %q", n, l)
		}
	}
}

// resetEnv isolates HOME and CLAUDE_CONFIG_DIR so --restore renders only into a scratch dir.
func resetEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestConfigureRestoreCheckExitCodes(t *testing.T) {
	// --out keeps the check on one scratch file (no live Windows-profile drift on WSL, no vendor homes).
	out := filepath.Join(resetEnv(t), "settings.json")
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--out", out}) }); code != 0 {
		t.Fatalf("--restore exit %d: %s", code, stderr)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--check", "--out", out}) }); code != 0 {
		t.Fatalf("--restore --check on a fresh render exit %d, want 0: %s", code, stderr)
	}
	path := out
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["model"] = "drifted-model"
	edited, _ := json.Marshal(m)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--check", "--out", out}) }); code != 1 {
		t.Errorf("--restore --check after editing model exit %d, want 1", code)
	}
}

func TestConfigureRestoreRejectsBadCombos(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"restore with page", []string{"--restore", "provider"}, "take no page"},
		{"revert with page", []string{"--revert", "agents"}, "take no page"},
		{"restore and revert", []string{"--restore", "--revert"}, "mutually exclusive"},
		{"revert with check", []string{"--revert", "--check"}, "--revert takes only"},
		{"revert with out", []string{"--revert", "--out", "x.json"}, "--revert takes only"},
		{"revert with agents", []string{"--revert", "--agents"}, "--revert takes only"},
		{"out without restore", []string{"--out", "x.json"}, "need --restore"},
		{"agents without restore", []string{"--agents"}, "need --restore"},
		{"check and dry-run", []string{"--restore", "--check", "--dry-run"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := captureOutput(t, func() int { return cmdConfigure(tc.args) })
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

// TestConfigureRestoreAgentCommaListScopesVendors: --agent a,b splits on the comma and renders only those vendors.
func TestConfigureRestoreAgentCommaListScopesVendors(t *testing.T) {
	resetEnv(t)
	code, out, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"--restore", "--dry-run", "--agents", "--agent", "codex,opencode"})
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, " (codex) ---") || !strings.Contains(out, " (opencode) ---") {
		t.Errorf("comma list did not render both vendors:\n%s", out)
	}
	for _, other := range []string{"copilot", "gemini"} {
		if strings.Contains(out, " ("+other+") ---") {
			t.Errorf("--agent codex,opencode also rendered %s:\n%s", other, out)
		}
	}
}

func TestConfigureRestoreBareWritesLiveSettings(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--restore"}) })
	if code != 0 {
		t.Fatalf("bare --restore exit %d: %s", code, stderr)
	}
	got, err := os.ReadFile(live)
	if err != nil || string(got) != string(want) {
		t.Fatalf("settings.json != render (err %v):\n%s", err, got)
	}
	_, after, ok := strings.Cut(stdout, "(backup ")
	if !ok {
		t.Fatalf("no backup reported:\n%s", stdout)
	}
	bk := strings.TrimSuffix(strings.SplitN(after, ")", 2)[0], " ")
	if old, err := os.ReadFile(bk); err != nil || string(old) != `{"stale":true}` {
		t.Errorf("backup %q missing or wrong (err %v): %s", bk, err, old)
	}
}

func TestConfigureRestoreDryRunLeavesLiveUntouched(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(live, past, past); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--dry-run"}) }); code != 0 {
		t.Fatalf("--restore --dry-run exit %d: %s", code, stderr)
	}
	got, _ := os.ReadFile(live)
	fi, err := os.Stat(live)
	if err != nil || string(got) != `{"stale":true}` || !fi.ModTime().Equal(past) {
		t.Errorf("dry-run touched live settings: %s mtime %v (want %v)", got, fi.ModTime(), past)
	}
}

func TestConfigureRestoreBareCheckDoesNotWrite(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--check"}) }); code != 1 {
		t.Errorf("--restore --check on a stale file exit %d, want 1", code)
	}
	if got, _ := os.ReadFile(live); string(got) != `{"stale":true}` {
		t.Errorf("--check wrote the file: %s", got)
	}
}

// TestConfigureRestoreSpansDefaultsOverlayAndHistory: defaults + vault overlay (a catalog key and an env
// variable), three hand-changed live keys, then --restore leaves exactly the render and a "before restore" snapshot.
func TestConfigureRestoreSpansDefaultsOverlayAndHistory(t *testing.T) {
	dir := vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	ov := map[string]json.RawMessage{
		"prefersReducedMotion": json.RawMessage("false"),
		"DISABLE_TELEMETRY":    json.RawMessage(`"1"`),
	}
	if err := settings.SaveOverlay(ov); err != nil {
		t.Fatal(err)
	}
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	var live map[string]json.RawMessage
	if err := json.Unmarshal(want, &live); err != nil {
		t.Fatal(err)
	}
	live["model"] = json.RawMessage(`"drifted"`)
	live["prefersReducedMotion"] = json.RawMessage("true")
	live["zzExtra"] = json.RawMessage("1")
	edited, _ := json.Marshal(live)
	file := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if keys, err := settings.DefaultsDrift(); err != nil || !slices.Equal(keys, []string{"model", "prefersReducedMotion", "zzExtra"}) {
		t.Fatalf("DefaultsDrift before = %v, %v", keys, err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--restore", "--no-agents"}) })
	if code != 0 {
		t.Fatalf("--restore exit %d: %s", code, stderr)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != string(want) {
		t.Fatalf("settings.json != render (err %v):\n%s", err, got)
	}
	if !strings.Contains(string(got), `"DISABLE_TELEMETRY": "1"`) {
		t.Errorf("overlay env variable missing from the restored file:\n%s", got)
	}
	log, err := (&history.Store{Dir: history.Dir()}).Log(1, "config")
	if err != nil || len(log) != 1 || !strings.Contains(log[0], "before restore") {
		t.Errorf("history log = %v, %v; want a before-restore snapshot", log, err)
	}
	if !strings.Contains(stdout, "before restore") || !strings.Contains(stdout, "restored "+file) {
		t.Errorf("stdout lacks the snapshot and restored lines:\n%s", stdout)
	}
	if keys, err := settings.DefaultsDrift(); err != nil || len(keys) != 0 {
		t.Errorf("DefaultsDrift after = %v, %v; want none", keys, err)
	}
}
