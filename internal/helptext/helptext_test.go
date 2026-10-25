package helptext

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPageLayout(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Bool("dry-run", false, "print only; no-op for --check and e-mail-style text")
	fs.String("format", "text", "output `kind`: text|json")
	fs.Int("n", 0, "count")
	fs.String("out", "", "destination")
	fs.Duration("timeout", 0, "wait duration")
	fs.String("very-long-flag-name-for-the-label", "", "long label description")
	p := Page{
		Usage:    "claude-code demo [flags]",
		About:    strings.Repeat("A descriptive sentence about this command. ", 5),
		Commands: [][2]string{{"show", "show the current configuration"}},
		Flags:    fs,
		Hint:     "claude-code demo show -h lists that page's flags.\n",
	}
	p.Fprint(&buf)
	got := buf.String()
	if !strings.HasPrefix(got, "usage: claude-code demo [flags]\n\n") {
		t.Errorf("missing opening synopsis:\n%s", got)
	}
	for _, want := range []string{
		"\n\nCOMMANDS\n", "\n\nFLAGS\n", "  show", "--format kind",
		`output kind: text|json (default "text")`, "no-op for --check and e-mail-style",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(got), " "), "no-op for --check and e-mail-style text") {
		t.Errorf("description changed while wrapping:\n%s", got)
	}
	fs.VisitAll(func(f *flag.Flag) {
		count := 0
		label := "--" + f.Name
		if utf8.RuneCountInString(f.Name) == 1 {
			label = "-" + f.Name
		}
		for _, line := range strings.Split(got, "\n") {
			words := strings.Fields(line)
			if len(words) > 0 && words[0] == label {
				count++
				if f.Name != "format" && strings.Contains(line, "(default") {
					t.Errorf("zero-valued flag has a default: %q", line)
				}
			}
		}
		if count != 1 {
			t.Errorf("--%s has %d rows, want 1", f.Name, count)
		}
	})
	if !strings.Contains(got, "--very-long-flag-name-for-the-label string  long label description") {
		t.Errorf("overlong label lacks two-space separator:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line has %d columns: %q", n, line)
		}
	}
	about := strings.Split(got, "\n\n")[1]
	if strings.Join(strings.Fields(about), " ") != strings.TrimSpace(p.About) {
		t.Errorf("About changed while wrapping: %q", about)
	}
	if !strings.HasSuffix(got, "\n\n"+p.Hint) {
		t.Errorf("Hint is not the last section:\n%s", got)
	}
}

func TestPageOmitsEmptySections(t *testing.T) {
	var out bytes.Buffer
	Page{Usage: "claude-code demo"}.Fprint(&out)
	if got, want := out.String(), "usage: claude-code demo\n"; got != want {
		t.Errorf("page = %q, want %q", got, want)
	}
}

func TestNewFlagSetParse(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		name string
		args []string
		code int
		done bool
	}{
		{"short help", []string{"-h"}, 0, true},
		{"long help", []string{"--help"}, 0, true},
		{"bad flag", []string{"--missing"}, 2, true},
		{"bad value", []string{"--wait=invalid"}, 2, true},
		{"valid", []string{"--quiet"}, 0, false},
		{"empty", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := NewFlagSet("demo", Page{Usage: "claude-code demo [flags]"})
			fs.Bool("quiet", false, "less output")
			fs.Duration("wait", time.Second, "wait duration")
			var buf bytes.Buffer
			fs.SetOutput(&buf)
			if fs.ErrorHandling() != flag.ContinueOnError {
				t.Errorf("ErrorHandling = %v, want ContinueOnError", fs.ErrorHandling())
			}
			if code, done := Parse(fs, tc.args); code != tc.code || done != tc.done {
				t.Errorf("Parse = (%d, %t), want (%d, %t)", code, done, tc.code, tc.done)
			}
			if tc.done && !strings.Contains(buf.String(), "usage: claude-code demo [flags]") {
				t.Errorf("parse omitted help:\n%s", &buf)
			}
			if tc.code == 0 && tc.done && strings.Count(buf.String(), "--quiet") != 1 {
				t.Errorf("help must list each registered flag once:\n%s", &buf)
			}
		})
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"help", flag.ErrHelp, 0},
		{"wrapped help", fmt.Errorf("parse: %w", flag.ErrHelp), 0},
		{"bad flag", errors.New("flag provided but not defined"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
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
