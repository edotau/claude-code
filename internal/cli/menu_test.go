package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
)

// The README's menu image is the current menu: a verb added or reworded without `make help-svg` fails the gate.
func TestMenuSVGIsCurrent(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", menuSVGPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, menuSVG()) {
		t.Fatalf("%s is stale: run `make help-svg` and commit it", menuSVGPath)
	}
}

func TestMenuSVGReportsStdoutFailure(t *testing.T) {
	hosttest.ClosedStdout(t)
	if got := cmdMenuSVG(nil); got == 0 {
		t.Fatal("help-svg exit = 0 on closed stdout, want nonzero")
	}
}

func captureUsage(t *testing.T) string {
	t.Helper()
	_, out, _ := hosttest.CaptureOutput(t, func() int { usage(os.Stdout); return 0 })
	return out
}

// Every rendered usage line fits 80 columns (barring one unbreakable word) and continuations start at the description column.
func TestUsageRowsWrapAtDescriptionColumn(t *testing.T) {
	out := captureUsage(t)
	const menuWidth = 80
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
	out := captureUsage(t)
	for _, verb := range []string{"network", "vault"} {
		if !regexp.MustCompile(`(?m)^  ` + verb + ` +\S`).MatchString(menuGroup(out, "AUTH")) {
			t.Errorf("%s row missing from AUTH:\n%s", verb, out)
		}
	}
	if !regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SESSION")) {
		t.Errorf("memory row missing from SESSION:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^  memory +\S`).MatchString(menuGroup(out, "SETTINGS")) {
		t.Errorf("memory still listed under SETTINGS")
	}
}

// AUTH, SESSION, DIAGNOSTICS and SETTINGS list verbs A→Z; ANTHROPIC keeps its tier order.
func TestMenuGroupsAlphabetical(t *testing.T) {
	byGroup := map[string][]string{}
	group := ""
	for _, r := range menuRows() {
		if r.head != "" {
			group = r.head
		} else if r.name != "" {
			byGroup[group] = append(byGroup[group], strings.TrimSpace(r.name))
		}
	}
	for g := range alphabetical {
		if !slices.IsSorted(byGroup[g]) {
			t.Errorf("%s not alphabetical: %v", g, byGroup[g])
		}
	}
	if got := byGroup[groupAgents]; len(got) == 0 || got[0] != "anthropic" {
		t.Errorf("AGENTS must open with anthropic, got %v", got)
	}
}
