package ask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// personaFixture writes agents/tester.md and other.md under a temp config dir with no agent binary on PATH.
func personaFixture(t *testing.T) (other string) {
	t.Helper()
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
	other = filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("---\ndescription: By path\n---\nYou are by path.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return other
}

// askLoggedRequest runs Cmd against the missing agent binary and returns the request it logged.
func askLoggedRequest(t *testing.T, args ...string) agent.RunOptions {
	t.Helper()
	if code, _, stderr := captureOutput(t, func() int { return Cmd(args) }); code != 1 {
		t.Fatalf("exit = %d, want 1 for unavailable agent; stderr=%s", code, stderr)
	}
	return agent.RecentRecords(1)[0].Request
}

func assertTesterPersona(t *testing.T, p *agent.Schema) {
	t.Helper()
	if p == nil || p.Name != "tester" || p.Description != "Tests things" || p.Prompt != "You test." || p.Model != "sonnet" ||
		len(p.Tools) != 2 || p.Tools[0] != "Read" {
		t.Fatalf("named persona = %+v", p)
	}
}

// --agent vendor:persona loads agents/<name>.md or a path into the logged request, and --retry replays it without the flag.
func TestAskPersonaLoadsAndRetryKeepsIt(t *testing.T) {
	other := personaFixture(t)
	ask := func(args ...string) agent.RunOptions { return askLoggedRequest(t, args...) }
	base := []string{"--fallback", "none", "--format", "json"}
	p := ask(append(base, "--agent", "claude:tester", "go")...).Persona
	assertTesterPersona(t, p)
	if p := ask(append(base, "--agent", "claude:"+other, "go")...).Persona; p == nil || p.Name != "other" || p.Prompt != "You are by path." {
		t.Fatalf("path persona = %+v", p)
	}
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "again", Persona: p}, time.Now().Add(-time.Hour))
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if got := ask(append(base, "--retry", rec.ID)...).Persona; got == nil || got.Name != "tester" {
		t.Fatalf("retry persona = %+v, want tester", got)
	}
	code, _, stderr := captureOutput(t, func() int { return Cmd(append(base, "--agent", "claude:nope", "go")) })
	if code != 2 || stderr == "" {
		t.Fatalf("unknown persona: exit %d stderr %q, want 2 with a message", code, stderr)
	}
}

// --as is checked against the resolved agent: a claude persona passes, any other agent exits 2.
func TestAskAsChecksResolvedAgent(t *testing.T) {
	personaFixture(t)
	base := []string{"--fallback", "none", "--format", "json", "--as", "sp"}
	run := func(sel string) (int, string) {
		code, _, stderr := captureOutput(t, func() int { return Cmd(append(append([]string{}, base...), "--agent", sel, "go")) })
		return code, stderr
	}
	if code, stderr := run("claude:tester"); code != 1 || strings.Contains(stderr, "applies to claude only") {
		t.Errorf("claude persona: exit %d stderr %q, want 1 (agent unavailable) with --as accepted", code, stderr)
	}
	for _, sel := range []string{"codex", "codex:tester"} {
		if code, stderr := run(sel); code != 2 || !strings.Contains(stderr, "applies to claude only") {
			t.Errorf("--agent %s --as sp: exit %d stderr %q, want 2 with \"applies to claude only\"", sel, code, stderr)
		}
	}
}
