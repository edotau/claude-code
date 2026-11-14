package ask

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

func TestMemoryRunOptionsNeverFallBack(t *testing.T) {
	o := memoryRunOptions(context.Background(), "p", "/w")
	if o.Fallback != router.FallbackNone || o.Agent != "claude" || !o.Tools || !o.Gate || o.WorkDir != "/w" {
		t.Fatalf("memoryRunOptions = %+v, want claude with tools and FallbackNone", o)
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
