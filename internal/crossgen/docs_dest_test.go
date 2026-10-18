package crossgen

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDocsHarness builds a minimal harness (CLAUDE.md + one agent) under an isolated CLAUDE_CONFIG_DIR.
func newDocsHarness(t *testing.T) (home, claude string) {
	t.Helper()
	home, claude = isolate(t)
	writeFile(t, filepath.Join(claude, "CLAUDE.md"), "# CLAUDE.md\n\nBody.\n\n\n")
	writeFile(t, filepath.Join(claude, "agents", "demo-agent.md"),
		"---\nname: demo-agent\ndescription: demo agent for the docs test. More words.\nmodel: sonnet\n---\n\nBody.\n")
	return home, claude
}

// The instruction render lands in each vendor's GLOBAL file (what its CLI reads from $HOME) plus the local projection
// copy, byte-identical, and never at the harness root — Claude Code would load a root AGENTS.md twice.
func TestSyncRendersVendorInstructionFilesNotRootCopies(t *testing.T) {
	home, claude := newDocsHarness(t)
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	want := DocsBanner + "\n\n# CLAUDE.md\n\nBody.\n\n## Agents\n\n- **demo-agent** (`sonnet`) — demo agent for the docs test.\n"
	for vendor, rel := range instructionFiles {
		dist := filepath.Join(ProjectionDir(claude, vendor), "..", filepath.Base(rel))
		for _, p := range []string{filepath.Join(home, rel), dist} {
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if string(got) != want {
				t.Errorf("%s mismatch\n got: %q\nwant: %q", p, got, want)
			}
		}
	}
	for _, name := range []string{"AGENTS.md", "GEMINI.md"} {
		if _, err := os.Stat(filepath.Join(claude, name)); err == nil {
			t.Errorf("%s was rendered at the harness root", name)
		}
	}
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check after a full sync = %d, want 0", code)
	}
}

// A hand-authored global file is kept; editing CLAUDE.md is drift --check reports in both dist and the home.
func TestInstructionRenderKeepsHandAuthoredAndGatesDrift(t *testing.T) {
	home, claude := newDocsHarness(t)
	hand := filepath.Join(home, ".codex", "AGENTS.md")
	writeFile(t, hand, "# my own codex instructions\n")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if got, _ := os.ReadFile(hand); !strings.HasPrefix(string(got), "# my own") {
		t.Errorf("hand-authored ~/.codex/AGENTS.md was overwritten: %q", got)
	}
	writeFile(t, filepath.Join(claude, "CLAUDE.md"), "# CLAUDE.md\n\nChanged.\n")
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code == 0 {
		t.Error("--check after a CLAUDE.md edit = 0, want drift")
	}
}

// A bare box (no CLAUDE.md) still syncs agents and skills, just without instruction renders.
func TestSyncWithoutClaudeMDSkipsInstructionRender(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "GEMINI.md")); err == nil {
		t.Error("GEMINI.md rendered without a CLAUDE.md source")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "agents", "demo.md")); err != nil {
		t.Errorf("agent projection missing: %v", err)
	}
}

// The render must never target CLAUDE.md itself — that would overwrite the source with its own derivative.
func TestInstructionFilesNeverTargetConventionsSource(t *testing.T) {
	for vendor, rel := range instructionFiles {
		if strings.EqualFold(filepath.Base(rel), "CLAUDE.md") {
			t.Fatalf("%s instruction file is %s — the render would clobber its own source", vendor, rel)
		}
	}
}
