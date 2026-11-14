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

// The memory job's `cd` rule reaches its bank directories (and below) and nothing beside or above them.
func TestMemoryRunOptionsCdCoversOnlyBankDirs(t *testing.T) {
	p := memoryRunOptions(context.Background(), "p", t.TempDir()).Permissions
	store := filepath.Join(paths.ConfigDir(), "memory")
	home := filepath.Join(store, "home")
	for cmd, want := range map[string]agent.Decision{
		"cd " + home: agent.Allow, "cd " + home + "/sub": agent.Allow,
		"cd " + store: agent.Deny, "cd " + store + "/other-bank": agent.Deny, "cd " + home + "-evil": agent.Deny,
		"cd " + home + "/../other-bank": agent.Deny, "cd /": agent.Deny, "cd": agent.Deny,
	} {
		if got := p.Decide("Bash", cmd); got != want {
			t.Errorf("Decide(Bash, %q) = %v, want %v", cmd, got, want)
		}
	}
}
