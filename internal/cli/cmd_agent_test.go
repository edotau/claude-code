package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent/agentcmd"
)

// TestVendorUsageFits80: every rendered line stays within an 80-column terminal.
func TestVendorUsageFits80(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		usage := renderHelpPage(agentcmd.AgentPage(vendor))
		for line := range strings.SplitSeq(strings.TrimRight(usage, "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > 80 {
				t.Errorf("vendorUsage(%q) line %q is %d runes, want <= 80", vendor, line, n)
			}
		}
	}
}

// TestVendorUsageNamesTail: every vendor's help block carries a tail row.
func TestVendorUsageNamesTail(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		if !strings.Contains(renderHelpPage(agentcmd.AgentPage(vendor)), "tail") {
			t.Errorf("vendorUsage(%q) has no tail row", vendor)
		}
	}
}

// The per-vendor help names the harness profile pin and shows the NAME placeholder on the launch --profile flag.
func TestVendorHelpHarnessProfileRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, vendor := range []string{"codex", "gemini"} {
		text := renderHelpPage(agentcmd.AgentPage(vendor))
		if !strings.Contains(text, "show or set "+vendor+"'s harness profile pin") {
			t.Errorf("%s help lacks the harness profile pin row:\n%s", vendor, text)
		}
		want := "harness profile for this launch"
		if vendor == "codex" {
			want += " (codex's own: -p NAME)"
		}
		var row string
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, "--profile NAME") {
				row = line
			}
		}
		if row == "" {
			t.Fatalf("%s help has no `--profile NAME` flag row:\n%s", vendor, text)
		}
		if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
			t.Errorf("%s help lacks flag usage %q:\n%s", vendor, want, text)
		}
		checkCommandRows(t, text)
		for line := range strings.SplitSeq(text, "\n") {
			if w := termWidth(line); w > 80 {
				t.Errorf("%s help line %q has %d columns, want <=80", vendor, line, w)
			}
		}
	}
}
