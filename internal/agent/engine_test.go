package agent

import (
	"errors"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// A hub cell with no opencode Hub linked into the binary is a dispatch error, not a nil-pointer panic.
func TestHubDispatchWithoutHubIsAnError(t *testing.T) {
	orig := hub
	hub = nil
	t.Cleanup(func() { hub = orig })
	if _, _, err := hubDispatchFor(&providers.Registry{}, "opencode-go/kimi"); !errors.Is(err, errNoHub) {
		t.Fatalf("hubDispatchFor err = %v, want errNoHub", err)
	}
	if _, err := hubStart(&providers.Registry{}, "claude", ""); !errors.Is(err, errNoHub) {
		t.Fatalf("hubStart err = %v, want errNoHub", err)
	}
}
