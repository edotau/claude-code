package doctor

import (
	"bytes"
	"testing"
)

// TestPrintChecksGoldenBytes pins printChecks' byte format (color off, the case for every test and
// any non-tty run): "<mark> <name>: <detail>\n", mark = ✓ when OK else ✗. Doctor and printVault both
// share this function now; a drift here changes what both commands print.
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
