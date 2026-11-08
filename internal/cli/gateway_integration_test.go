package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestDesktopAndTokenHelpFitsEightyColumns(t *testing.T) {
	for _, args := range [][]string{{"configure", "desktop", "--help"}, {"gateway", "token", "--help"}} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return Run(append([]string{"claude-code"}, args...)) })
		if code != 0 || stdout != "" {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
		for line := range strings.SplitSeq(stderr, "\n") {
			if termWidth(line) > 80 {
				t.Errorf("%v: line exceeds 80 columns: %s", args, line)
			}
		}
	}
}
