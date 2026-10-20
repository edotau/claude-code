package cli

import (
	"os"
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
	for _, line := range strings.Split(stdout, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "claude" {
			t.Errorf("usage advertises hidden claude verb: %q", line)
		}
	}
}

// Every menu row fits an 80-column terminal, every group is present, and every alias resolves.
func TestMenuRowsFitAndAliasesResolve(t *testing.T) {
	want := map[string]bool{groupAnthropic: false, groupVendors: false, groupAuth: false, groupSession: false, groupDiagnostics: false, groupSettings: false}
	for _, c := range commands() {
		if n := utf8.RuneCountInString(c.summary); n > summaryMax {
			t.Errorf("%s: summary is %d chars (max %d): %q", c.name, n, summaryMax, c.summary)
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
	for _, name := range []string{"token", "providers", "use", "settings"} {
		if lookup(name) == nil {
			t.Errorf("legacy name %s no longer dispatches", name)
		}
	}
}
