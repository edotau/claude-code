package launch

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// RouteDetail is the per-vendor shim `vendoragent setup` calls instead of running the whole doctor
// sweep; its detail string must match the corresponding "<v> route" row in DoctorChecks exactly.
func TestRouteDetailMatchesDoctorChecks(t *testing.T) {
	doctorScratch(t)
	t.Setenv("GH_TOKEN", "ghp_doctorFAKEtoken1234567890")
	setPins(t, map[string]string{providers.SlotPin("sonnet"): "gpt-5.3-codex"})

	checks, _ := DoctorChecks()
	for _, vendor := range []string{"codex", "gemini", "opencode", "copilot"} {
		detail, ok := RouteDetail(vendor)
		if !ok {
			t.Errorf("RouteDetail(%q) ok = false, want true", vendor)
			continue
		}
		c := findCheck(checks, vendor+" route")
		if c == nil {
			t.Fatalf("no %q check in DoctorChecks output", vendor+" route")
		}
		if detail != c.Detail {
			t.Errorf("RouteDetail(%q) = %q, want DoctorChecks' %q", vendor, detail, c.Detail)
		}
	}
}

// An unknown vendor reports ok=false, never a stale/zero detail string.
func TestRouteDetailUnknownVendor(t *testing.T) {
	doctorScratch(t)
	if detail, ok := RouteDetail("not-a-vendor"); ok || detail != "" {
		t.Errorf("RouteDetail(unknown) = (%q, %v), want (\"\", false)", detail, ok)
	}
}
