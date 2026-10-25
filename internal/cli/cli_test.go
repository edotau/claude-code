package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestRunDoesNotDispatchArgvZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, stdout, stderr := captureOutput(t, func() int { return Run([]string{"codex"}) })
	if code != 0 || stderr != "" {
		t.Fatalf("Run exit = %d, stderr = %q, want 0 and no error", code, stderr)
	}
	_, want, _ := captureOutput(t, func() int { usage(os.Stdout); return 0 })
	if stdout != want {
		t.Errorf("Run with argv[0]=codex output = %q, want usage %q", stdout, want)
	}
}

func TestHiddenClaudeVerbDispatchesButIsAbsentFromUsage(t *testing.T) {
	cliScratch(t)
	bin := pathstest.FakeVendor(t, "claude", "exit 99")
	c := lookup("claude")
	if c == nil || !c.hidden {
		t.Fatal("claude must be a registered hidden command")
	}
	code, stdout, stderr := captureOutput(t, func() int {
		return Run([]string{"claude-code", "claude", "--provider", "subscription", "--print-env"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("claude --print-env exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "binary   "+bin+"\n") {
		t.Errorf("claude --print-env did not resolve the vendor binary:\n%s", stdout)
	}
	code, stdout, stderr = captureOutput(t, func() int { return Run([]string{"claude-code", "help"}) })
	if code != 0 || stderr != "" {
		t.Fatalf("help exit = %d, stderr = %q", code, stderr)
	}
	for line := range strings.SplitSeq(stdout, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "claude" {
			t.Errorf("usage advertises hidden claude verb: %q", line)
		}
	}
}

// Every group is present, every group is present, and every alias resolves.
func TestMenuRowsFitAndAliasesResolve(t *testing.T) {
	want := map[string]bool{groupAnthropic: false, groupVendors: false, groupAuth: false, groupSession: false, groupDiagnostics: false, groupSettings: false}
	for _, c := range commands() {
		if strings.TrimSpace(c.summary) == "" {
			t.Errorf("%s: empty summary", c.name)
		}
		if !c.hidden {
			if _, ok := want[c.group]; !ok {
				t.Errorf("%s: unknown group %q", c.name, c.group)
			}
			want[c.group] = true
		}
		for _, a := range c.aliases {
			if got := lookup(a); got == nil || got.name != c.name {
				t.Errorf("alias %s does not resolve to %s", a, c.name)
			}
		}
	}
	for g, seen := range want {
		if !seen {
			t.Errorf("group %s has no rows", g)
		}
	}
	for _, name := range []string{"token", "use"} {
		if lookup(name) == nil {
			t.Errorf("legacy name %s no longer dispatches", name)
		}
		for _, name := range []string{"providers"} {
			if lookup(name) != nil {
				t.Errorf("removed provider-list verb %s still dispatches", name)
			}
		}
	}
}

// Every rendered usage line fits 80 columns (barring one unbreakable word) and continuations start at the description column.
func TestUsageRowsWrapAtDescriptionColumn(t *testing.T) {
	_, out, _ := captureOutput(t, func() int { usage(os.Stdout); return 0 })
	col := 2 + nameWidth
	for line := range strings.SplitSeq(out, "\n") {
		if utf8.RuneCountInString(line) > menuWidth && len(strings.Fields(line)) > 2 {
			t.Errorf("line over %d cols: %q", menuWidth, line)
		}
		if ind := len(line) - len(strings.TrimLeft(line, " ")); ind > 10 && ind != col {
			t.Errorf("continuation indent %d, want %d: %q", ind, col, line)
		}
	}
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(c.name) + ` +\S`).MatchString(out) {
			t.Errorf("row for %s missing or misaligned", c.name)
		}
	}
}

func menuGroup(out, group string) string {
	i := strings.Index(out, "\n"+group+"\n")
	if i < 0 {
		return ""
	}
	rest := out[i+len(group)+2:]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestMenuRegroupNetworkAuthMemorySession(t *testing.T) {
	_, out, _ := captureOutput(t, func() int { usage(os.Stdout); return 0 })
	if !regexp.MustCompile(`(?m)^  network +\S`).MatchString(menuGroup(out, "AUTH")) {
		t.Errorf("network row missing from AUTH:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SESSION")) {
		t.Errorf("memory row missing from SESSION:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SETTINGS")) {
		t.Errorf("memory still listed under SETTINGS")
	}
	for _, old := range []string{"tailscale", "web", "mcp", "generate", "vscode", "settings", "statusline"} {
		if regexp.MustCompile(`(?m)^  ` + old + ` +\S`).MatchString(out) {
			t.Errorf("old verb %q still in the menu", old)
		}
	}
}

func TestOldVerbsAreUnknownCommands(t *testing.T) {
	for _, old := range []string{"tailscale", "web", "mcp", "generate", "vscode", "settings", "statusline"} {
		if lookup(old) != nil {
			t.Errorf("lookup(%q) still resolves", old)
		}
		code, _, stderr := captureOutput(t, func() int { return Run([]string{"claude-code", old}) })
		if code != 2 || !strings.Contains(stderr, "unknown command") {
			t.Errorf("%s: exit %d, stderr %q, want exit 2 unknown command", old, code, stderr)
		}
	}
}
