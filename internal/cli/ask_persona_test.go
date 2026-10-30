package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// --persona loads agents/<name>.md or a path into the logged request, and --retry replays it without the flag.
func TestAskPersonaLoadsAndRetryKeepsIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir) // no agent binary: the run fails after the record is saved
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	def := "---\nname: tester\ndescription: Tests things\ntools: Read, Grep\nmodel: sonnet\n---\nYou test.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "tester.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("---\ndescription: By path\n---\nYou are by path.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ask := func(args ...string) agent.Request {
		t.Helper()
		if code, _, stderr := captureOutput(t, func() int { return cmdAsk(args) }); code != 1 {
			t.Fatalf("exit = %d, want 1 for unavailable runner; stderr=%s", code, stderr)
		}
		return agent.RecentRecords(1)[0].Request
	}
	base := []string{"--agent", "claude", "--fallback", "none", "--format", "json"}
	p := ask(append(base, "--persona", "tester", "go")...).Persona
	if p == nil || p.Name != "tester" || p.Description != "Tests things" || p.Prompt != "You test." || p.Model != "sonnet" ||
		len(p.Tools) != 2 || p.Tools[0] != "Read" {
		t.Fatalf("named persona = %+v", p)
	}
	if p := ask(append(base, "--persona", other, "go")...).Persona; p == nil || p.Name != "other" || p.Prompt != "You are by path." {
		t.Fatalf("path persona = %+v", p)
	}
	rec := agent.NewRecord("claude", agent.Request{Prompt: "again", Persona: p}, time.Now().Add(-time.Hour))
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if got := ask(append(base, "--retry", rec.ID)...).Persona; got == nil || got.Name != "tester" {
		t.Fatalf("retry persona = %+v, want tester", got)
	}
	code, _, stderr := captureOutput(t, func() int { return cmdAsk(append(base, "--persona", "nope", "go")) })
	if code != 2 || stderr == "" {
		t.Fatalf("unknown persona: exit %d stderr %q, want 2 with a message", code, stderr)
	}
}
