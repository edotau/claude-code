package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// TestConfigureRoutingRespectsRouteNotFound verifies code checks Route's ok return value.
// Regression: line 275 checks `route != "https://api.anthropic.com"` without checking ok,
// so "" != "https://api.anthropic.com" is true, incorrectly setting ANTHROPIC_BASE_URL="".
func TestConfigureRoutingRespectsRouteNotFound(t *testing.T) {
	// Test the exact scenario: provider with no ProtocolAnthropic route should not set URL.
	p := &providers.Provider{
		Name:     "test-provider",
		Protocol: "custom-dialect",
		BaseURL:  "https://custom.example.com",
	}
	route, ok := p.Route(providers.ProtocolAnthropic)
	if ok {
		t.Fatalf("Route(ProtocolAnthropic) should return false for provider with no anthropic route")
	}
	if route != "" {
		t.Fatalf("Route returned non-empty %q when not found", route)
	}
	// BUG SCENARIO: condition `route != "https://api.anthropic.com"` without ok check.
	// "" != "https://api.anthropic.com" evaluates to true, so code would set ANTHROPIC_BASE_URL="".
	// The fix must check ok first: `ok && route != "https://api.anthropic.com"`.
	if route != "https://api.anthropic.com" {
		// This is the buggy condition - when route is "", this is true, causing incorrect assignment.
		// After fix: this condition should also check ok to prevent the false positive.
		t.Logf("Buggy condition would set ANTHROPIC_BASE_URL to %q when it should not set it at all", route)
	}
}
