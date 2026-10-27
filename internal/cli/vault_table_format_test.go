package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/secrets"
)

func TestVaultSecretRowsAlignPreviewAndRightAlignLength(t *testing.T) {
	rows := vaultSecretRows([]secrets.Stored{
		{Name: "A", Prefix: "sk-ant-", Len: 108},
		{Name: "B", Prefix: "", Len: 7},
		{Name: "C", Prefix: "abcde", Len: 25},
	})
	want := []string{"sk-ant-***  108 chars", "***           7 chars", "abcde***     25 chars"}
	for i, r := range rows {
		if r[1] != want[i] {
			t.Errorf("row %d = %q, want %q", i, r[1], want[i])
		}
		if utf8.RuneCountInString(r[1]) != utf8.RuneCountInString(rows[0][1]) {
			t.Errorf("row %d = %q is not the same width as row 0", i, r[1])
		}
		if strings.Contains(r[1], "(") {
			t.Errorf("row %d = %q: length must not be parenthesised", i, r[1])
		}
	}
}

func TestVaultTruncateMiddleKeepsBothEnds(t *testing.T) {
	a := vaultTruncateMiddle("hooks.PreToolUse.matchers[0].command", 24)
	b := vaultTruncateMiddle("hooks.PreToolUse.matchers[1].command", 24)
	if a == b {
		t.Fatalf("distinct keys collapsed to %q", a)
	}
	if utf8.RuneCountInString(a) != 24 || !strings.Contains(a, "…") || !strings.HasPrefix(a, "hooks.") || !strings.HasSuffix(a, ".command") {
		t.Errorf("vaultTruncateMiddle = %q, want 24 runes with both ends and an ellipsis", a)
	}
	if got := vaultTruncateMiddle("short", 24); got != "short" {
		t.Errorf("short key = %q, want unchanged", got)
	}
	if got := vaultTruncateMiddle("héllo-wörld-ünïcode", 8); utf8.RuneCountInString(got) != 8 {
		t.Errorf("multibyte clip = %q, want 8 runes", got)
	}
}

func TestPresetEnvsFailuresFirstWithLeadingMarks(t *testing.T) {
	old := vaultCredOK
	t.Cleanup(func() { vaultCredOK = old })
	vaultCredOK = func(name string) bool { return name != "qa" && name != "zeta" }
	got := presetEnvs([]string{"prod", "zeta", "qa", "dev"})
	if want := "✗qa ✗zeta ✓dev ✓prod"; got != want {
		t.Errorf("presetEnvs = %q, want %q", got, want)
	}
}

func TestVaultTableCapsColumnsWithEllipsis(t *testing.T) {
	out := vaultTable([]string{"NAME", "KIND"}, []int{6, 0}, [][]string{
		{"abcdefghijkl", "a-very-long-uncapped-kind"},
		{"abc", "x"},
	}, 200)
	if !strings.Contains(out, "abcde…") || strings.Contains(out, "abcdef") {
		t.Errorf("capped column not clipped to 6 runes with …:\n%s", out)
	}
	if !strings.Contains(out, "a-very-long-uncapped-kind") {
		t.Errorf("uncapped column was clipped:\n%s", out)
	}
	if !strings.Contains(out, "abc ") {
		t.Errorf("short cell altered:\n%s", out)
	}
}
