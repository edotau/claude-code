package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestHelpScreensFitEightyColumns(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	cases := []struct {
		name string
		run  func() int
	}{
		{"network -h", func() int { return cmdNetwork([]string{"-h"}) }},
		{"network vscode -h", func() int { return cmdNetwork([]string{"vscode", "-h"}) }},
		{"theme -h", func() int { return cmdTheme([]string{"-h"}) }},
		{"configure -h", func() int { return cmdConfigure([]string{"-h"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, stderr := captureOutput(t, tc.run)
			if strings.TrimSpace(stderr) == "" {
				t.Fatal("help printed nothing to stderr")
			}
			for _, l := range strings.Split(stderr, "\n") {
				if n := utf8.RuneCountInString(l); n > 80 {
					t.Errorf("%d runes: %q", n, l)
				}
				if n := termWidth(l); n > 80 {
					t.Errorf("%d cols (tabs expanded): %q", n, l)
				}
			}
		})
	}
}

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"-h"}) })
	if !strings.Contains(stderr, "--reset") {
		t.Errorf("help lacks --reset:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -reset") {
		t.Errorf("help prints single-dash -reset:\n%s", stderr)
	}
}
