package agentcmd

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// catalog reaches cmdVendorModels, not launchVendor, with canonical diagnostics and help.
func TestVendorVerbRoutesCatalogToCmdVendorModels(t *testing.T) {
	for _, sub := range []string{"catalog"} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{sub, "unexpected"}) })
		if code != 2 || !strings.Contains(stderr, "codex catalog: unexpected argument") {
			t.Errorf("vendorVerb(%s): code=%d stderr=%q", sub, code, stderr)
		}
		code, _, stderr = hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{sub, "--help"}) })
		if code != 0 || !strings.HasPrefix(stderr, "usage: claude-code codex catalog") {
			t.Errorf("vendorVerb(%s --help): code=%d stderr=%q", sub, code, stderr)
		}
	}
}
