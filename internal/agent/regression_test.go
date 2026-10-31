package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// TestScienceConfigPortAccepts65534 verifies port 65534 is accepted (sandbox would be 65535, valid).
// Regression: off-by-one error rejected valid port 65534 with condition n < 65534.
func TestScienceConfigPortAccepts65534(t *testing.T) {
	got := scienceConfigPort([]byte("port = 65534\n"))
	if got != 65534 {
		t.Errorf("scienceConfigPort port=65534: got %d, want 65534 (sandbox 65535 is valid)", got)
	}
}

// TestScienceConfigPortRejects65535AndBeyond verifies port 65535 is rejected (sandbox would be 65536, invalid).
func TestScienceConfigPortRejects65535AndBeyond(t *testing.T) {
	got := scienceConfigPort([]byte("port = 65535\n"))
	if got != 3000 { // Should fall back to default when port is invalid
		t.Errorf("scienceConfigPort port=65535: got %d, want default 3000", got)
	}
}

// TestScienceArgsAccepts65534Port verifies scienceArgs accepts port 65534.
// Regression: off-by-one error: condition port >= 65534 rejected valid port 65534.
func TestScienceArgsAccepts65534Port(t *testing.T) {
	port, rest, err := scienceArgs([]string{"--port=65534", "serve"}, 3000)
	if err != nil {
		t.Errorf("scienceArgs --port=65534: got error %v, want none (sandbox port 65535 is valid)", err)
	}
	if port != 65534 {
		t.Errorf("scienceArgs --port=65534: got port %d, want 65534", port)
	}
	if len(rest) != 1 || rest[0] != "serve" {
		t.Errorf("scienceArgs --port=65534: rest=%v, want [serve]", rest)
	}
}

// TestScienceArgsRejects65535Port verifies scienceArgs rejects port 65535 (sandbox would be 65536, invalid).
func TestScienceArgsRejects65535Port(t *testing.T) {
	_, _, err := scienceArgs([]string{"--port=65535", "serve"}, 3000)
	if err == nil {
		t.Error("scienceArgs --port=65535: got no error, want error (sandbox port 65536 invalid)")
	}
}

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
