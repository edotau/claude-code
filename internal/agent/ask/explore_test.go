package ask

import (
	"slices"
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

// Prompt must name all five required sections, cite file:line, and carry the question verbatim.
func TestExplorePromptHasFiveSections(t *testing.T) {
	got := explorePrompt("how does X work?", false)
	assertContainsAll(t, got, "Structure", "Conventions", "Dependencies", "Prior art", "Open questions",
		"file:line", "how does X work?")
}

// web=false must omit the web-research dispatch entirely (no URL-citation instruction).
func TestExplorePromptOmitsWebWhenDisabled(t *testing.T) {
	got := explorePrompt("q", false)
	if strings.Contains(got, "URL") || strings.Contains(got, "web") {
		t.Errorf("web=false prompt must not mention web research:\n%s", got)
	}
}

// web=true appends the web-research dispatch, which cites claims by URL.
func TestExplorePromptIncludesWebWhenEnabled(t *testing.T) {
	got := explorePrompt("q", true)
	assertContainsAll(t, got, "web", "URL")
}

// ExploreArgs includes --tools exactly for vendors whose ReadNeedsTools is true.
func TestExploreArgsToolsFlagPerVendor(t *testing.T) {
	cases := []struct {
		vendor    string
		wantTools bool
	}{
		{"codex", false},
		{"copilot", true},
	}
	for _, c := range cases {
		args, err := ExploreArgs(c.vendor, "/work", "q", false, false)
		if err != nil {
			t.Fatal(err)
		}
		got := slices.Contains(args, "--tools")
		if got != c.wantTools {
			t.Errorf("ExploreArgs(%q) --tools present = %v, want %v (args=%v)", c.vendor, got, c.wantTools, args)
		}
	}
}

// ExploreArgs includes --background exactly when requested, independent of the vendor.
func TestExploreArgsBackgroundFlag(t *testing.T) {
	fg, err := ExploreArgs("codex", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fg, "--background") {
		t.Errorf("foreground ExploreArgs must not include --background: %v", fg)
	}
	bg, err := ExploreArgs("codex", "/work", "q", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(bg, "--background") {
		t.Errorf("background ExploreArgs must include --background: %v", bg)
	}
}

// ExploreArgs always carries --agent/--workdir and ends with the built prompt as the last argv element.
func TestExploreArgsShape(t *testing.T) {
	args, err := ExploreArgs("copilot", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 3 || args[0] != "--agent" || args[1] != "copilot" {
		t.Fatalf("ExploreArgs must start with --agent copilot: %v", args)
	}
	if !slices.Contains(args, "--workdir") {
		t.Errorf("ExploreArgs must include --workdir: %v", args)
	}
	last := args[len(args)-1]
	if last != explorePrompt("q", false) {
		t.Errorf("ExploreArgs must end with the built prompt, got %q", last)
	}
}

func TestExploreToolsReadOnly(t *testing.T) {
	cases := []struct {
		vendor       string
		wantTools    bool
		wantReadOnly bool
	}{
		{"copilot", true, true},
		{"codex", false, false},
	}
	for _, c := range cases {
		t.Run(c.vendor, func(t *testing.T) {
			args, err := ExploreArgs(c.vendor, "/work", "inspect the repository", false, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(args, "--tools"); got != c.wantTools {
				t.Errorf("ExploreArgs --tools = %v, want %v", got, c.wantTools)
			}
			if got := slices.Contains(args, "--read-only"); got != c.wantReadOnly {
				t.Errorf("ExploreArgs --read-only = %v, want %v", got, c.wantReadOnly)
			}
		})
	}
}

func TestExploreRejectsPromptOnlyAgents(t *testing.T) {
	for _, vendor := range []string{"gemini", "opencode"} {
		t.Run(vendor, func(t *testing.T) {
			want := "explore on " + vendor + " needs native read-only support, which it emulates with permission denies; use codex or copilot"
			args, err := ExploreArgs(vendor, "/work", "inspect", true, true)
			if err == nil {
				t.Fatal("explore accepted a prompt-only agent")
			}
			if err.Error() != want {
				t.Fatalf("explore error = %v; want %q", err, want)
			}
			if args != nil {
				t.Fatalf("ExploreArgs returned runnable arguments: %v", args)
			}
		})
	}
}
