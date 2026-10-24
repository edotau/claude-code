package cli

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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
