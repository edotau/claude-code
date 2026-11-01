package explore

import (
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/codex" // the vendors register their schemas from their own init
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
)

func assertContainsAll(t *testing.T, got string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(got, s) {
			t.Errorf("prompt missing %q:\n%s", s, got)
		}
	}
}

// Prompt must name all five required sections, cite file:line, and carry the question verbatim.
func TestExplorePromptHasFiveSections(t *testing.T) {
	got := Prompt("how does X work?", false)
	assertContainsAll(t, got, "Structure", "Conventions", "Dependencies", "Prior art", "Open questions",
		"file:line", "how does X work?")
}

// web=false must omit the web-research dispatch entirely (no URL-citation instruction).
func TestExplorePromptOmitsWebWhenDisabled(t *testing.T) {
	got := Prompt("q", false)
	if strings.Contains(got, "URL") || strings.Contains(got, "web") {
		t.Errorf("web=false prompt must not mention web research:\n%s", got)
	}
}

// web=true appends the web-research dispatch, which cites claims by URL.
func TestExplorePromptIncludesWebWhenEnabled(t *testing.T) {
	got := Prompt("q", true)
	assertContainsAll(t, got, "web", "URL")
}

// Args includes --tools exactly for vendors whose ReadNeedsTools is true.
func TestExploreArgsToolsFlagPerVendor(t *testing.T) {
	cases := []struct {
		vendor    string
		wantTools bool
	}{
		{"codex", false},
		{"gemini", true},
		{"copilot", true},
		{"opencode", true},
	}
	for _, c := range cases {
		args := Args(c.vendor, "/work", "q", false, false)
		got := argvContains(args, "--tools")
		if got != c.wantTools {
			t.Errorf("Args(%q) --tools present = %v, want %v (args=%v)", c.vendor, got, c.wantTools, args)
		}
	}
}

// Args includes --background exactly when requested, independent of the vendor.
func TestExploreArgsBackgroundFlag(t *testing.T) {
	fg := Args("codex", "/work", "q", false, false)
	if argvContains(fg, "--background") {
		t.Errorf("foreground Args must not include --background: %v", fg)
	}
	bg := Args("codex", "/work", "q", false, true)
	if !argvContains(bg, "--background") {
		t.Errorf("background Args must include --background: %v", bg)
	}
}

// Args always carries --agent/--workdir and ends with the built prompt as the last argv element.
func TestExploreArgsShape(t *testing.T) {
	args := Args("gemini", "/work", "q", false, false)
	if len(args) < 3 || args[0] != "--agent" || args[1] != "gemini" {
		t.Fatalf("Args must start with --agent gemini: %v", args)
	}
	if !argvContains(args, "--workdir") {
		t.Errorf("Args must include --workdir: %v", args)
	}
	last := args[len(args)-1]
	if last != Prompt("q", false) {
		t.Errorf("Args must end with the built prompt, got %q", last)
	}
}

func argvContains(args []string, flag string) bool {
	return slices.Contains(args, flag)
}
