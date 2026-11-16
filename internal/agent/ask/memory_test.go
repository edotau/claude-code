package ask

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
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
