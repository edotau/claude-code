package launch

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// A vendor the harness cannot route still launches its own CLI untouched, after one configure offer.
func TestVendorPassesThroughWhenUnroutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HARNESS_PROVIDER", "subscription") // passthrough auth: gemini cannot use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	fake := pathstest.FakeVendor(t, "agy", "")
	offers := 0
	OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { OfferConfigure = nil })

	plan, err := Vendor(context.Background(), "gemini", Options{}, []string{"auth", "login"})
	if err != nil {
		t.Fatalf("expected a passthrough plan, got %v", err)
	}
	// gemini's own built-in vendor spec (vendorconfig defaults) still prepends --dangerously-skip-permissions.
	wantArgv := []string{"--dangerously-skip-permissions", "auth", "login"}
	if plan.Binary != fake || !slices.Equal(plan.Argv, wantArgv) || len(plan.Set) != 0 {
		t.Errorf("plan = %+v, want the plain CLI with %v prepended and no harness env", plan, wantArgv)
	}
	if offers != 1 {
		t.Errorf("configure offered %d times, want 1", offers)
	}
	// --print-env never prompts.
	offers = 0
	if _, err := Vendor(context.Background(), "gemini", Options{PrintEnv: true}, nil); err != nil || offers != 0 {
		t.Errorf("print-env: err=%v offers=%d", err, offers)
	}
}
