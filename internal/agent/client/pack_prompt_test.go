package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/router"
)

func gitRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	names := []string{}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	for _, args := range [][]string{{"init", "-q"}, append([]string{"add"}, names...)} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// Given a tracked file and a tracked .env, Prepare packs the file only and records the task, not the bytes.
func TestPreparePacksAndKeepsRecordCompact(t *testing.T) {
	setupWorkdirFixture(t)
	dir := gitRepoWith(t, map[string]string{"main.go": "package main // NEEDLE-1", ".env": "TOKEN=hunter2"})
	var notes []string
	client := NewClient(router.RuleClassifier{}.Alias, nil, func(s string) { notes = append(notes, s) })
	p, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "explain", WorkDir: dir,
		Fallback: router.FallbackNone, Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if !strings.Contains(p.Opts.Prompt, "NEEDLE-1") || strings.Contains(p.Opts.Prompt, "hunter2") {
		t.Errorf("prompt must hold main.go and not .env:\n%s", p.Opts.Prompt)
	}
	if p.Record.Request.Prompt != "explain" || p.Record.Request.Pack == nil {
		t.Errorf("record = %q pack=%v, want the bare task and the pack spec", p.Record.Request.Prompt, p.Record.Request.Pack)
	}
}

func TestPrepareRefusesPackWithHost(t *testing.T) {
	setupWorkdirFixture(t)
	client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	_, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "p", Host: "box",
		Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("Prepare() = %v, want a --host refusal", err)
	}
}

// Given a lead with a 1M window and a stub on a 200K model, an n between their budgets drops only the stub.
func TestFitChainSkipsSmallWindows(t *testing.T) {
	lead := agent.Dispatch{Agent: stubEngine("claude"), Model: "claude-sonnet-5-5[1m]"}
	small := agent.Dispatch{Agent: stubEngine("stub"), Model: "mystery-model"}
	chain := agent.Chain{Dispatches: []agent.Dispatch{lead, small}}
	lb, _ := packBudget(lead, agent.RunOptions{}, false)
	sb, _ := packBudget(small, agent.RunOptions{}, false)
	if sb >= lb {
		t.Fatalf("fixture: small budget %d must be under lead %d", sb, lb)
	}
	got := fitChain(chain, (lb+sb)/2, agent.RunOptions{})
	if len(got.Dispatches) != 1 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "stub") {
		t.Fatalf("chain = %+v, want only the lead and a skip naming stub", got)
	}
	if kept := fitChain(chain, sb-1, agent.RunOptions{}); len(kept.Dispatches) != 2 {
		t.Errorf("a prompt under both budgets must keep both, got %+v", kept)
	}
}

func TestPackBudgetArgvCap(t *testing.T) {
	d := agent.Dispatch{Agent: stubEngine("opencode"), Model: "claude-sonnet-5-5[1m]"}
	if got, note := packBudget(d, agent.RunOptions{}, false); got != agent.MaxPromptBytes || !strings.Contains(note, "argv prompt cap") {
		t.Errorf("budget = %d (%q), want MaxPromptBytes with a cap note", got, note)
	}
}

// A PackFile lead keeps the prompt to the instruction, turns tools on, and its file never re-enters a Collect.
func TestPackPromptFileTransport(t *testing.T) {
	setupWorkdirFixture(t)
	body := strings.Repeat("x", 300<<10)
	dir := gitRepoWith(t, map[string]string{"big.txt": body, ".env": "TOKEN=hunter2"})
	lead := agent.Dispatch{Agent: stubEngine("gemini"), Model: "claude-sonnet-5-5[1m]"}
	p := &Prepared{Opts: agent.RunOptions{Prompt: "find it", WorkDir: dir, Pack: &pack.Spec{Dirs: []string{"."}, MaxFileBytes: 400 << 10}},
		Chain: agent.Chain{Dispatches: []agent.Dispatch{lead}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(p.Opts.Prompt) > 1024 || !strings.Contains(p.Opts.Prompt, p.pack.path) || !p.Opts.Tools || !p.Record.Request.Tools {
		t.Fatalf("prompt = %q tools=%v, want a short instruction naming %s with tools on", p.Opts.Prompt, p.Opts.Tools, p.pack.path)
	}
	if len(p.pack.body) < 300<<10 || strings.Contains(p.pack.body, "hunter2") {
		t.Fatalf("pack body = %d bytes, want the 300 KB file and no .env", len(p.pack.body))
	}
	if err := p.pack.write(); err != nil {
		t.Fatal(err)
	}
	got, err := pack.Collect(context.Background(), dir, []string{"."}, nil, 100, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Included {
		if strings.HasPrefix(f.Path, ".claude-code-pack-") {
			t.Errorf("Collect picked up the pack file %s", f.Path)
		}
	}
	p.pack.remove()
	if _, err := os.Stat(p.pack.path); !os.IsNotExist(err) {
		t.Errorf("pack file still present after remove: %v", err)
	}
}

// Given a gemini lead (pack read from a file with tools), --read-only is refused before any launch.
func TestPackPromptRefusesReadOnlyOnFileRoute(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	o := agent.RunOptions{Prompt: "p", WorkDir: dir, ReadOnly: true, Pack: &pack.Spec{Dirs: []string{"."}}}
	p := &Prepared{Opts: o, Chain: agent.Chain{Dispatches: []agent.Dispatch{{Agent: stubEngine("gemini")}}}}
	err := NewClient(router.RuleClassifier{}.Alias, nil, nil).packPrompt(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "--read-only") {
		t.Fatalf("packPrompt() = %v, want a --read-only refusal", err)
	}
}

// Given a relative --workdir, the file-route pack path in the prompt is absolute (the child starts inside the workdir).
func TestPackPromptFilePathAbsoluteForRelativeWorkdir(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	t.Chdir(dir)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{Opts: agent.RunOptions{Prompt: "p", WorkDir: "sub", Pack: &pack.Spec{Dirs: []string{"."}}},
		Chain: agent.Chain{Dispatches: []agent.Dispatch{{Agent: stubEngine("gemini")}}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("sub")
	if want := filepath.Join(abs, pack.FilePrefix+"id1.md"); p.pack.path != want || !strings.Contains(p.Opts.Prompt, want) {
		t.Fatalf("pack path = %q, prompt = %q, want absolute %q", p.pack.path, p.Opts.Prompt, want)
	}
}

// stubEngine is an agent.Engine that only has a name; the pack tests never run it.
type stubEngine string

func (s stubEngine) Name() string     { return string(s) }
func (s stubEngine) Caps() agent.Caps { return agent.Caps{} }
func (s stubEngine) Available() error { return nil }
func (s stubEngine) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}
