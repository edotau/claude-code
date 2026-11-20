package copilot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// Copilot carries system text natively: both caps are Native (so the prompt stays unfolded) and System is filled.
func TestSystemPromptRidesSDKNotPrompt(t *testing.T) {
	if row.CLI.Caps.SystemPrompt != agent.Native || row.CLI.Caps.AppendSystemPrompt != agent.Native {
		t.Fatalf("caps = %v/%v, want Native/Native", row.CLI.Caps.SystemPrompt, row.CLI.Caps.AppendSystemPrompt)
	}
	req := agent.RunOptions{Prompt: "say hi", SystemPrompt: "be ZEBRA", AppendSystemPrompt: "end with ZEBRA"}
	req.Tools = true
	o, err := newSDKOptions("copilot", nil, req, agent.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if o.System != "be ZEBRA\n\nend with ZEBRA" {
		t.Errorf("System = %q, want both texts", o.System)
	}
}

// The workdir's CLAUDE.md already inside the rendered copilot-instructions.md is sent once, not discovered twice.
func TestSameInstructions(t *testing.T) {
	const core = "# CLAUDE.md\n\nthe portable core\n"
	setup := func(t *testing.T, rendered string, extra ...string) (home, work string) {
		t.Helper()
		home, work = t.TempDir(), t.TempDir()
		files := map[string]string{filepath.Join(home, "copilot-instructions.md"): rendered, filepath.Join(work, "CLAUDE.md"): core}
		for _, e := range extra {
			files[filepath.Join(work, e)] = "x"
		}
		for p, body := range files {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return home, work
	}
	rendered := "<!-- GENERATED -->\n" + core + "\n# rules\n"
	home, work := setup(t, rendered)
	if got := sameInstructions(home, work); got != rendered {
		t.Errorf("duplicate CLAUDE.md: got %q, want the rendered file", got)
	}
	home, work = setup(t, "<!-- GENERATED -->\n# something else\n")
	if got := sameInstructions(home, work); got != "" {
		t.Errorf("a CLAUDE.md the rendered file lacks must keep discovery, got %q", got)
	}
	for _, other := range []string{"AGENTS.md", filepath.Join(".github", "copilot-instructions.md")} {
		home, work = setup(t, rendered, other)
		if got := sameInstructions(home, work); got != "" {
			t.Errorf("%s beside CLAUDE.md must keep discovery, got %q", other, got)
		}
	}
	if got := sameInstructions(home, t.TempDir()); got != "" {
		t.Errorf("a workdir with no CLAUDE.md must keep discovery, got %q", got)
	}
}
