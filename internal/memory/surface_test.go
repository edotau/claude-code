package memory

import (
	"strings"
	"testing"
)

// The Parked section leaves the injected surface (with a count), the other sections stay, and recall can still find it.
func TestSessionSurfaceDropsParked(t *testing.T) {
	ac := "# Active Context\n\n## Current Focus\n\n- shipping the router\n\n## Parked\n\n- home network gateway firmware\n- interview prep notes\n\n## Blockers\n\n- none\n"
	got := dropParked(ac)
	for _, want := range []string{"shipping the router", "## Blockers", "2 parked item(s) omitted"} {
		if !strings.Contains(got, want) {
			t.Errorf("surface lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gateway firmware") || strings.Contains(got, "## Parked") {
		t.Errorf("parked content leaked into the surface:\n%s", got)
	}
	if in := "# A\n\n## Current Focus\n\n- x\n"; dropParked(in) != in {
		t.Error("a file without Parked must pass through unchanged")
	}
}
