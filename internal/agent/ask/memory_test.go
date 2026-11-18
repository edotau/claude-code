package ask

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

func TestMemoryRunOptionsNeverFallBack(t *testing.T) {
	o := memoryRunOptions(context.Background(), "p", "/w")
	if o.Fallback != router.FallbackNone || o.Agent != "claude" || !o.Tools || !o.Gate || o.WorkDir != "/w" {
		t.Fatalf("memoryRunOptions = %+v, want claude with tools and FallbackNone", o)
	}
}

// A dream run's context widens the memory job to every repo store; an update stays on its own bank.
func TestMemoryRunOptionsFollowTheJobKind(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	work := t.TempDir()
	other := filepath.Join(memory.StoreDir(), "other-repo", "progress.md")
	if got := memoryRunOptions(t.Context(), "p", work).Permissions.Decide("Write", other); got == agent.Allow {
		t.Errorf("update may write another repo's bank")
	}
	var dream context.Context
	run := func(ctx context.Context, _, _ string) (string, error) { dream = ctx; return "stub", nil }
	if err := memory.RunDream(memory.DreamRunOptions{CWD: work, Force: true, Out: io.Discard, Run: run}); err != nil || dream == nil {
		t.Fatalf("RunDream ran the agent = %v, err %v", dream != nil, err)
	}
	if got := memoryRunOptions(dream, "p", work).Permissions.Decide("Write", other); got != agent.Allow {
		t.Errorf("dream Decide(Write, another repo's bank) = %v, want allow", got)
	}
}

// The memory job's `cd` rule reaches the store (and below) and nothing beside or above it.
func TestMemoryRunOptionsCdCoversOnlyTheStore(t *testing.T) {
	p := memoryRunOptions(context.Background(), "p", t.TempDir()).Permissions
	store := filepath.Join(paths.ConfigDir(), "memory")
	for cmd, want := range map[string]agent.Decision{
		"cd " + store: agent.Allow, "cd " + store + "/archive": agent.Allow,
		"cd " + store + "-evil": agent.Deny, "cd " + store + "/../other": agent.Deny,
		"cd " + filepath.Dir(store): agent.Deny, "cd /": agent.Deny, "cd": agent.Deny,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}

// The memory job's Read denies are the settings template's spellings and refuse secret files through Bash.
func TestMemoryRunOptionsDenySecretReads(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	p := memoryRunOptions(context.Background(), "p", t.TempDir()).Permissions
	for _, s := range []string{"Read(**/state/vault.db)", "Read(**/.credentials.json)", "Read(**/.env)", "Read(**/*.pem)"} {
		if r, err := agent.ParseRule(s); err != nil || !slices.Contains(p.Deny, r) {
			t.Errorf("deny %s: parsed %+v, %v; not in %v", s, r, err, p.Deny)
		}
	}
	cd := "cd " + memory.StoreDir() + " && "
	for _, f := range []string{cfg + "/state/vault.db", cfg + "/.credentials.json", cfg + "/.env", cfg + "/k.pem"} {
		for _, cmd := range []string{"cat " + f, cd + "head -1 " + f, "grep x " + f + " | wc -l", cd + "cat $(echo " + f + ")"} {
			if got := p.Decide("Bash", cmd); got != agent.Deny {
				t.Errorf("Decide(Bash, %q) = %v, want deny", cmd, got)
			}
		}
	}
	if got := p.Decide("Bash", cd+"cat ../state/vault.db"); got != agent.Deny {
		t.Errorf("climbing read = %v, want deny", got)
	}
}

// The memory job may run compound read-only inspection in the store; writes and git writes stay refused.
func TestMemoryRunOptionsReadOnlyBash(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := memoryRunOptions(context.Background(), "p", t.TempDir()).Permissions
	cd := "cd " + memory.StoreDir() + " && "
	for cmd, want := range map[string]agent.Decision{
		cd + "wc -l claude-code--progress.md && head -n 20 claude-code--progress.md && echo done":           agent.Allow,
		cd + "head -30 claude-code--sessionHistory.md && echo ---- && head -30 claude-code--decisionLog.md": agent.Allow,
		cd + "sed -n 5,9p claude-code--activeContext.md; git log --oneline -5 | grep -v '^$'":               agent.Allow,
		cd + "tail -5 claude-code--conventions.md":                                                          agent.Allow,
		"echo x > /tmp/f": agent.Deny,
		cd + "cat claude-code--progress.md > /tmp/out":             agent.Deny,
		cd + "git add claude-code--progress.md && git commit -m x": agent.Deny,
		"git commit -m x":                      agent.Deny,
		"echo ok; rm -rf " + memory.StoreDir(): agent.Deny,
		"echo $(git commit -m x)":              agent.Deny,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}

// An ask brief gains one recall block, keeps a held one unchanged, and a retry replays the sent bytes.
func TestAskAttachesRecallOnce(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "HOME", "PATH"} {
		t.Setenv(k, dir)
	}
	task := "stage the deploy pipeline staging signoff rollout"
	run := func(args ...string) string {
		hosttest.CaptureOutput(t, func() int {
			return Cmd(append([]string{"--agent", "claude", "--fallback", "none", "--format", "json"}, args...))
		})
		return agent.RecentRecords(1)[0].Request.Prompt
	}
	if bare := run(task); bare != task {
		t.Fatalf("empty bank: prompt = %q, want the bare task", bare)
	}
	bareID := agent.RecentRecords(1)[0].ID
	var b strings.Builder
	for i := range 3 {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
	}
	bank := filepath.Join(memory.StoreDir(), "conventions.md")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bank, []byte("# Conventions\n\n"+b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	first := run(task)
	if strings.Count(first, "<memory-recall>") != 1 || !strings.HasPrefix(first, task) {
		t.Fatalf("prompt = %q, want the task plus one block", first)
	}
	id := agent.RecentRecords(1)[0].ID
	if held := run(first); held != first {
		t.Errorf("a brief holding a block changed: %q", held)
	}
	if again := run("--retry", id); again != first {
		t.Errorf("retry sent %q, want %q", again, first)
	}
	if again := run("--retry", bareID); again != task {
		t.Errorf("retry of a bare send = %q, want %q", again, task)
	}
	if off := run("--safe-mode", task); off != task {
		t.Errorf("--safe-mode prompt = %q, want the bare task", off)
	}
}
