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

	"github.com/edotau/claude-code/internal/settings"
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
	for _, l := range strings.Split(stderr, "\n") {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("help line is %d cols: %q", n, l)
		}
	}
}

// resetEnv isolates HOME and CLAUDE_CONFIG_DIR so --reset renders only into a scratch dir.
func resetEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestConfigureResetCheckExitCodes(t *testing.T) {
	// --out keeps the check on one scratch file (no live Windows-profile drift on WSL, no vendor homes).
	out := filepath.Join(resetEnv(t), "settings.json")
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--reset", "--out", out}) }); code != 0 {
		t.Fatalf("--reset exit %d: %s", code, stderr)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--reset", "--check", "--out", out}) }); code != 0 {
		t.Fatalf("--reset --check on a fresh render exit %d, want 0: %s", code, stderr)
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
	if code, _, _ := captureOutput(t, func() int { return cmdConfigure([]string{"--reset", "--check", "--out", out}) }); code != 1 {
		t.Errorf("--reset --check after editing model exit %d, want 1", code)
	}
}

func TestConfigureResetRejectsBadCombos(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"reset with page", []string{"--reset", "provider"}, "take no page"},
		{"restore with page", []string{"--restore", "agents"}, "take no page"},
		{"reset and restore", []string{"--reset", "--restore"}, "mutually exclusive"},
		{"restore with check", []string{"--restore", "--check"}, "--restore takes only"},
		{"restore with out", []string{"--restore", "--out", "x.json"}, "--restore takes only"},
		{"restore with vendors", []string{"--restore", "--vendors"}, "--restore takes only"},
		{"out without reset", []string{"--out", "x.json"}, "need --reset"},
		{"vendors without reset", []string{"--vendors"}, "need --reset"},
		{"check and dry-run", []string{"--reset", "--check", "--dry-run"}, "mutually exclusive"},
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

// TestConfigureResetAgentCommaListScopesVendors: --agent a,b splits on the comma and renders only those vendors.
func TestConfigureResetAgentCommaListScopesVendors(t *testing.T) {
	resetEnv(t)
	code, out, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"--reset", "--dry-run", "--vendors", "--agent", "codex,opencode"})
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

func TestConfigureResetBareWritesLiveSettings(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--reset"}) })
	if code != 0 {
		t.Fatalf("bare --reset exit %d: %s", code, stderr)
	}
	got, err := os.ReadFile(live)
	if err != nil || string(got) != string(want) {
		t.Fatalf("settings.json != render (err %v):\n%s", err, got)
	}
	i := strings.Index(stdout, "(backup ")
	if i < 0 {
		t.Fatalf("no backup reported:\n%s", stdout)
	}
	bk := strings.TrimSuffix(strings.SplitN(stdout[i+len("(backup "):], ")", 2)[0], " ")
	if old, err := os.ReadFile(bk); err != nil || string(old) != `{"stale":true}` {
		t.Errorf("backup %q missing or wrong (err %v): %s", bk, err, old)
	}
}

func TestConfigureResetDryRunLeavesLiveUntouched(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(live, past, past); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"--reset", "--dry-run"}) }); code != 0 {
		t.Fatalf("--reset --dry-run exit %d: %s", code, stderr)
	}
	got, _ := os.ReadFile(live)
	fi, err := os.Stat(live)
	if err != nil || string(got) != `{"stale":true}` || !fi.ModTime().Equal(past) {
		t.Errorf("dry-run touched live settings: %s mtime %v (want %v)", got, fi.ModTime(), past)
	}
}

func TestConfigureResetBareCheckDoesNotWrite(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdConfigure([]string{"--reset", "--check"}) }); code != 1 {
		t.Errorf("--reset --check on a stale file exit %d, want 1", code)
	}
	if got, _ := os.ReadFile(live); string(got) != `{"stale":true}` {
		t.Errorf("--check wrote the file: %s", got)
	}
}
