package launch

import (
	"bytes"
	"testing"
)

// TestPrintChecksGoldenBytes pins printChecks' byte format (color off, the case for every test and
// any non-tty run): "<mark> <name>: <detail>\n", mark = ✓ when OK else ✗. Doctor and VaultDoctor both
// share this function now; a drift here changes what both commands print.
func TestPrintChecksGoldenBytes(t *testing.T) {
	checks := []Check{
		{Name: "sample", OK: true, Detail: "all good"},
		{Name: "sample2", OK: false, Detail: "broken: reason"},
	}
	var buf bytes.Buffer
	printChecks(&buf, checks)
	want := "✓ sample: all good\n✗ sample2: broken: reason\n"
	if got := buf.String(); got != want {
		t.Errorf("printChecks =\n%q\nwant\n%q", got, want)
	}
}
