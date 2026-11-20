package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
)

func TestCapabilityRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        RunOptions
		capability string
	}{
		{"gemini", RunOptions{NoTools: true}, "--no-tools"},
		{"claude", RunOptions{Tools: true, NoTools: true}, "--tools"},
	} {
		err := CheckCapabilities(tc.name, tc.req)
		var failure *retry.Error
		if !errors.As(err, &failure) || failure.Kind != retry.KindFatal || !strings.Contains(err.Error(), tc.capability) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if err := CheckCapabilities("claude", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckCapabilities("codex", RunOptions{ReadOnly: true, Tools: true}); err != nil { // tools-off run, not a refusal
		t.Fatal(err)
	}
	if err := CheckCapabilities("codex", RunOptions{ReadOnly: true, NoTools: true}); err == nil {
		t.Fatal("codex has no --no-tools; the unsupported flag must still be refused")
	}
}

// Only --no-tools and permission features refuse; every other unsupported flag runs and is reported dropped.
func TestCapabilityDroppedFlagsStillRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RunOptions
	}{
		{"gemini", RunOptions{NoPersist: true}},
		{"opencode", RunOptions{NoPersist: true}},
		{"copilot", RunOptions{DebugFile: "/tmp/x.log"}},
		{"gemini", RunOptions{Fleet: true}},
		{"copilot", RunOptions{SafeMode: true}},
		{"codex", RunOptions{Fleet: true, Model: "m", Effort: "high", SafeMode: true}},
	} {
		if err := CheckCapabilities(tc.name, tc.req); err != nil {
			t.Errorf("%s refused; want run with the flag dropped: %v", tc.name, err)
		}
	}
}
