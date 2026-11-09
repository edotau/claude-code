package roles

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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
func TestPromptHasFiveSections(t *testing.T) {
	got := Prompt("how does X work?", false)
	assertContainsAll(t, got, "Structure", "Conventions", "Dependencies", "Prior art", "Open questions",
		"file:line", "how does X work?")
}

// web=false must omit the web-research dispatch entirely (no URL-citation instruction).
func TestPromptOmitsWebWhenDisabled(t *testing.T) {
	got := Prompt("q", false)
	if strings.Contains(got, "URL") || strings.Contains(got, "web") {
		t.Errorf("web=false prompt must not mention web research:\n%s", got)
	}
}

// web=true appends the web-research dispatch, which cites claims by URL.
func TestPromptIncludesWebWhenEnabled(t *testing.T) {
	got := Prompt("q", true)
	assertContainsAll(t, got, "web", "URL")
}

// Args includes --tools exactly for vendors whose ReadNeedsTools is true.
func TestArgsToolsFlagPerVendor(t *testing.T) {
	cases := []struct {
		vendor    string
		wantTools bool
	}{
		{"codex", false},
		{"copilot", true},
	}
	for _, c := range cases {
		args, err := Args(c.vendor, "/work", "q", false, false)
		if err != nil {
			t.Fatal(err)
		}
		got := argvContains(args, "--tools")
		if got != c.wantTools {
			t.Errorf("Args(%q) --tools present = %v, want %v (args=%v)", c.vendor, got, c.wantTools, args)
		}
	}
}

// Args includes --background exactly when requested, independent of the vendor.
func TestArgsBackgroundFlag(t *testing.T) {
	fg, err := Args("codex", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if argvContains(fg, "--background") {
		t.Errorf("foreground Args must not include --background: %v", fg)
	}
	bg, err := Args("codex", "/work", "q", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !argvContains(bg, "--background") {
		t.Errorf("background Args must include --background: %v", bg)
	}
}

// Args always carries --agent/--workdir and ends with the built prompt as the last argv element.
func TestArgsShape(t *testing.T) {
	args, err := Args("copilot", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 3 || args[0] != "--agent" || args[1] != "copilot" {
		t.Fatalf("Args must start with --agent copilot: %v", args)
	}
	if !argvContains(args, "--workdir") {
		t.Errorf("Args must include --workdir: %v", args)
	}
	last := args[len(args)-1]
	if last != Prompt("q", false) {
		t.Errorf("Args must end with the built prompt, got %q", last)
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
			args, err := Args(c.vendor, "/work", "inspect the repository", false, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(args, "--tools"); got != c.wantTools {
				t.Errorf("Args --tools = %v, want %v", got, c.wantTools)
			}
			if got := slices.Contains(args, "--read-only"); got != c.wantReadOnly {
				t.Errorf("Args --read-only = %v, want %v", got, c.wantReadOnly)
			}
			base := agent.RunOptions{Agent: c.vendor}
			options, err := Options(base, "inspect the repository", false)
			if err != nil {
				t.Fatal(err)
			}
			if options.Tools != c.wantTools || options.ReadOnly != c.wantReadOnly {
				t.Errorf("Options Tools/ReadOnly = %v/%v, want %v/%v",
					options.Tools, options.ReadOnly, c.wantTools, c.wantReadOnly)
			}
		})
	}
}

func TestExploreRejectsPromptOnlyAgents(t *testing.T) {
	for _, vendor := range []string{"gemini", "opencode"} {
		t.Run(vendor, func(t *testing.T) {
			want := "explore on " + vendor + " is read-only by prompt only; use codex or copilot (read-only enforced)"
			args, err := Args(vendor, "/work", "inspect", true, true)
			assertExploreRefusal(t, err, want)
			if args != nil {
				t.Fatalf("Args returned runnable arguments: %v", args)
			}
			_, err = Options(agent.RunOptions{Agent: vendor}, "inspect", true)
			assertExploreRefusal(t, err, want)
			r := Role{Name: "explore", Agent: vendor}
			legs := Legs(r, Request{Task: "inspect", WorkDir: "/work", Web: true})
			if len(legs) != 1 {
				t.Fatalf("want one refused leg, got %+v", legs)
			}
			if legs[0].Err != want {
				t.Fatalf("leg error = %q; want %q", legs[0].Err, want)
			}
			seen := fakeStream(t)
			rep := Run(context.Background(), nil, r, legs, 1, nil)
			if len(*seen) != 0 || rep.Status != "error" {
				t.Fatalf("refused explore dispatched: calls=%v report=%+v", *seen, rep)
			}
			if got := rep.Legs[0].Envelope.Error; got == nil || got.Message != want {
				t.Fatalf("refusal lost in report: %+v", got)
			}
		})
	}
}

func assertExploreRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("explore accepted a prompt-only agent")
	}
	if err.Error() != want {
		t.Fatalf("explore error = %v; want %q", err, want)
	}
}

func argvContains(args []string, flag string) bool {
	return slices.Contains(args, flag)
}
