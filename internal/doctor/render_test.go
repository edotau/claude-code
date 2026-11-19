package doctor

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

// TestPrintChecksGoldenBytes pins printChecks' byte format (color off, the case for every test and any non-tty run)
func TestPrintChecksGoldenBytes(t *testing.T) {
	checks := []check{
		{Name: "sample", OK: true, Detail: "all good"},
		{Name: "sample2", OK: false, Detail: "broken: reason"},
	}
	var buf bytes.Buffer
	printCheckLines(&buf, checks)
	want := "✓ sample: all good\n✗ sample2: broken: reason\n"
	if got := buf.String(); got != want {
		t.Errorf("printChecks =\n%q\nwant\n%q", got, want)
	}
}

// A soft row worded "warn" prints the ⚠ mark; a hard row or a plain soft failure keeps ✗.
func TestPrintCheckLinesWarnMark(t *testing.T) {
	checks := []check{
		{Name: "dead", Detail: "oauth · verified · warn user login dead (needs_login)"},
		{Name: "ver", Detail: "warn 1.0.100 below 1.0.104"},
		{Name: "hard", Hard: true, Detail: "warn x"},
		{Name: "path", Detail: "/opt/warn /bin"},
	}
	var buf bytes.Buffer
	printCheckLines(&buf, checks)
	want := "⚠ dead: oauth · verified · warn user login dead (needs_login)\n⚠ ver: warn 1.0.100 below 1.0.104\n" +
		"✗ hard: warn x\n✗ path: /opt/warn /bin\n"
	if got := buf.String(); got != want {
		t.Errorf("printCheckLines =\n%q\nwant\n%q", got, want)
	}
}

func TestPrintCheckLinesClipsPassingRowsOnATerminal(t *testing.T) {
	long := strings.Repeat("detail-", 20)
	cases := []struct {
		name  string
		width int
		c     check
		whole bool
		max   int
	}{
		{"piped passing row whole", 0, check{Name: "row", OK: true, Detail: long}, true, 0},
		{"tty passing row clipped to 80", 200, check{Name: "row", OK: true, Detail: long}, false, 80},
		{"tty warning row whole", 200, check{Name: "row", Detail: "warn " + long}, true, 0},
		{"tty failing row whole", 200, check{Name: "row", Hard: true, Detail: long}, true, 0},
		{"narrow tty passing row fits 40", 40, check{Name: "row", OK: true, Detail: long}, false, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := termWidth
			termWidth = func(io.Writer) int { return tc.width }
			t.Cleanup(func() { termWidth = old })
			var buf bytes.Buffer
			printCheckLines(&buf, []check{tc.c})
			line := strings.TrimSuffix(buf.String(), "\n")
			if tc.whole {
				if !strings.HasSuffix(line, tc.c.Detail) {
					t.Errorf("row clipped: %q", line)
				}
				return
			}
			if w := textutil.DisplayWidth(line); w > tc.max || !strings.HasSuffix(line, "...") {
				t.Errorf("width %d (max %d), line %q", w, tc.max, line)
			}
		})
	}
}
