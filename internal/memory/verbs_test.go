package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// embedIndex uses the profile it is handed, not a fresh session/pin lookup; reembedGate refuses an unsafe one.
func TestEmbedIndexAndGateUseTheResolvedProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "session")
	var opened string
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(_ context.Context, profile, _ string) (*EmbedClient, error) {
		opened = profile
		return nil, errors.New("stub")
	}
	embedIndex(&bytes.Buffer{}, &bytes.Buffer{}, t.TempDir(), "resolved", false)
	if opened != "resolved" {
		t.Fatalf("embedIndex opened %q, want the profile it was given", opened)
	}
	if err := reembedGate("../escape", time.Now()); err == nil {
		t.Fatal("reembedGate on an unsafe profile = nil, want an error")
	}
}
