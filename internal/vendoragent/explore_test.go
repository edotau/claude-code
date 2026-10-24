package vendoragent

import (
	"strings"
	"testing"
)

func assertContainsAll(t *testing.T, got string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(got, s) {
			t.Errorf("prompt missing %q:\n%s", s, got)
		}
	}
}

// ExplorePrompt must name all five required sections, cite file:line, and carry the question verbatim.
func TestExplorePromptHasFiveSections(t *testing.T) {
	got := ExplorePrompt("how does X work?", "/work", false)
	assertContainsAll(t, got, "Structure", "Conventions", "Dependencies", "Prior art", "Open questions",
		"file:line", "how does X work?")
}

// web=false must omit the web-research leg entirely (no URL-citation instruction).
func TestExplorePromptOmitsWebWhenDisabled(t *testing.T) {
	got := ExplorePrompt("q", "/work", false)
	if strings.Contains(got, "URL") || strings.Contains(got, "web") {
		t.Errorf("web=false prompt must not mention web research:\n%s", got)
	}
}

// web=true appends the web-research leg, which cites claims by URL.
func TestExplorePromptIncludesWebWhenEnabled(t *testing.T) {
	got := ExplorePrompt("q", "/work", true)
	assertContainsAll(t, got, "web", "URL")
}

// ExploreArgs includes --tools exactly for vendors whose ReadNeedsTools is true.
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
		args := ExploreArgs(c.vendor, "/work", "q", false, false)
		got := argvContains(args, "--tools")
		if got != c.wantTools {
			t.Errorf("ExploreArgs(%q) --tools present = %v, want %v (args=%v)", c.vendor, got, c.wantTools, args)
		}
	}
}

// ExploreArgs includes --background exactly when requested, independent of the vendor.
func TestExploreArgsBackgroundFlag(t *testing.T) {
	fg := ExploreArgs("codex", "/work", "q", false, false)
	if argvContains(fg, "--background") {
		t.Errorf("foreground ExploreArgs must not include --background: %v", fg)
	}
	bg := ExploreArgs("codex", "/work", "q", false, true)
	if !argvContains(bg, "--background") {
		t.Errorf("background ExploreArgs must include --background: %v", bg)
	}
}

// ExploreArgs always carries --agent/--workdir and ends with the built prompt as the last argv element.
func TestExploreArgsShape(t *testing.T) {
	args := ExploreArgs("gemini", "/work", "q", false, false)
	if len(args) < 3 || args[0] != "--agent" || args[1] != "gemini" {
		t.Fatalf("ExploreArgs must start with --agent gemini: %v", args)
	}
	if !argvContains(args, "--workdir") {
		t.Errorf("ExploreArgs must include --workdir: %v", args)
	}
	last := args[len(args)-1]
	if last != ExplorePrompt("q", "/work", false) {
		t.Errorf("ExploreArgs must end with the built prompt, got %q", last)
	}
}

func argvContains(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
