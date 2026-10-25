package helptext

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func TestDefaultsWidensEveryFlagToTwoDashes(t *testing.T) {
	var buf bytes.Buffer
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(&buf)
	fs.Bool("dry-run", false, "print only; no-op for --check and e-mail-style text")
	fs.String("format", "text", "output format: text|json")
	fs.Int("n", 3, "count")
	Defaults(fs)
	got := buf.String()
	for _, want := range []string{"  --dry-run\n", "  --format string\n", "  --n int\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "  -") && !strings.HasPrefix(l, "  --") {
			t.Errorf("single-dash flag line left: %q", l)
		}
	}
	if !strings.Contains(got, "no-op for --check and e-mail-style text") {
		t.Errorf("description dashes altered: %q", got)
	}
	if strings.Contains(got, "---") {
		t.Errorf("dashes doubled twice: %q", got)
	}
}

func TestNewFlagSetUsageIsThemedAndListsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	fs := NewFlagSet("demo", "usage: demo [flags]\n")
	fs.Bool("quiet", false, "less output")
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.Usage()
	got := buf.String()
	if !strings.Contains(got, "usage: demo [flags]") || !strings.Contains(got, "--quiet") {
		t.Errorf("usage = %q", got)
	}
	if fs.ErrorHandling() != flag.ContinueOnError {
		t.Errorf("ErrorHandling = %v, want ContinueOnError", fs.ErrorHandling())
	}
	if err := fs.Parse([]string{"-h"}); err != flag.ErrHelp {
		t.Errorf("Parse -h = %v, want ErrHelp", err)
	}
}

// Fprint paints only on a terminal; Paint is the forced path NewFlagSet's Usage goes through.
func TestPaintColorsDoubleDashFlagRows(t *testing.T) {
	painted := Paint("FLAGS\n  --quiet\n    \tless output\n")
	if !strings.Contains(painted, "\x1b[") {
		t.Errorf("Paint added no escape codes: %q", painted)
	}
	if !strings.Contains(painted, "--quiet") {
		t.Errorf("painted row lost its label: %q", painted)
	}
}
