package vendors

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

// A pinned gh user that is not logged in fails closed: no stored token, no active account, a named fix.
func TestCopilotPinMissingFailsClosed(t *testing.T) {
	copilotAuthEnv(t)
	if err := secrets.Store(CopilotTokenEnv, "stored-tok"); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinCopilotProfile: "ghost"}); err != nil {
		t.Fatal(err)
	}
	if a, ok := copilotAuthSource(); ok {
		t.Fatalf("a missing pinned user must not resolve another credential: got %+v", a)
	}
	tok, err := CopilotLaunchToken()
	if err == nil || tok != "" || !strings.Contains(err.Error(), "claude-code configure github") || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("CopilotLaunchToken = (%q, %v), want an error naming ghost and the fix", tok, err)
	}
	if _, _, err := copilotLogin("auto", nil); err == nil {
		t.Error("the launch env must fail too, not run copilot on its own stored login")
	}
}

// Unpinned behaviour is unchanged: gh's active account resolves and the launch token has no error.
func TestCopilotUnpinnedUnchanged(t *testing.T) {
	copilotAuthEnv(t)
	if err := CopilotPinError(); err != nil {
		t.Fatalf("no pin must mean no pin error: %v", err)
	}
	if a, ok := copilotAuthSource(); !ok || a.Source != srcActive || a.Account != "alice" {
		t.Fatalf("unpinned: got %+v, want gh's active account alice", a)
	}
}
