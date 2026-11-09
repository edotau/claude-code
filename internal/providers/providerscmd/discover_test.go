package providerscmd

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func TestPrintDiscoverResultsTextWriteError(t *testing.T) {
	results := []copilot.DiscoverResult{{Provider: "openai", Profile: "fake", Status: "preview", Discovered: 1}}
	for _, status := range []int{0, 1} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return printDiscoverResults(results, false, helptext.Text, status)
		})
		if code != status || !strings.Contains(stdout, "openai/fake: preview") || stderr != "" {
			t.Fatalf("successful write: code=%d, want %d; stdout=%q stderr=%q", code, status, stdout, stderr)
		}
	}
	code, stderr := captureClosedStdout(t, func() int {
		return printDiscoverResults(results, false, helptext.Text, 0)
	})
	if code != 1 || !strings.Contains(stderr, "models discover: cannot write text output") {
		t.Fatalf("failed text write: code=%d stderr=%q, want exit 1 and write diagnostic", code, stderr)
	}
}
