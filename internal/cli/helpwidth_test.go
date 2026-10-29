package cli

import (
	"strings"
	"testing"
)

// termWidth is a line's display width with tabs expanded to 8-column stops.
func termWidth(l string) int {
	w := 0
	for _, r := range l {
		if r == '\t' {
			w += 8 - w%8
		} else {
			w++
		}
	}
	return w
}

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"-h"}) })
	if !strings.Contains(stderr, "--restore") {
		t.Errorf("help lacks --restore:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -restore") {
		t.Errorf("help prints single-dash -restore:\n%s", stderr)
	}
}
