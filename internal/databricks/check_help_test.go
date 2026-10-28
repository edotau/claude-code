package databricks

import (
	"strings"
	"testing"
)

func TestCheckHelpShowsCheckPageNotTopLevel(t *testing.T) {
	code, out, errs := captureOutput(t, func() int { return Run([]string{"check", "--help"}) })
	all := out + errs
	if code != 0 || !strings.Contains(all, "databricks check [MODEL]") {
		t.Fatalf("check --help: exit %d, want check's own page:\n%s", code, all)
	}
	// Negative control: the plain page is the launcher's.
	_, out, errs = captureOutput(t, func() int { return Run([]string{"--help"}) })
	if strings.Contains(out+errs, "databricks check [MODEL]") {
		t.Errorf("top-level help shows check's usage:\n%s", out+errs)
	}
}
