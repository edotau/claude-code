package helptext

import (
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// timeoutExplicit distinguishes a --timeout the caller typed from the flag's own zero-cost default,
// so the --tools-aware 45m default never clobbers an explicit value.
func TestTimeoutExplicit(t *testing.T) {
	newFS := func() (*flag.FlagSet, *time.Duration) {
		fs := flag.NewFlagSet("ask", flag.ContinueOnError)
		return fs, fs.Duration("timeout", 10*time.Minute, "")
	}
	fs, _ := newFS()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if Explicit(fs, "timeout") {
		t.Error("timeoutExplicit() = true with no flags parsed, want false")
	}

	fs, timeout := newFS()
	if err := fs.Parse([]string{"-timeout", "5m"}); err != nil {
		t.Fatal(err)
	}
	if !Explicit(fs, "timeout") {
		t.Error("timeoutExplicit() = false with -timeout on the command line, want true")
	}
	if *timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", *timeout)
	}
}

func captureStderr(t *testing.T, f func() int) (string, int) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, f)
	return stderr, code
}

// BadFormat must print the prefixed one-line error and exit 2; Fail exits 1.
func TestFailExitCodes(t *testing.T) {
	out, code := captureStderr(t, func() int { return BadFormat("vault ls", errors.New("bad")) })
	if out != "claude-code: vault ls: bad\n" || code != 2 {
		t.Errorf("BadFormat = %q, %d", out, code)
	}
	out, code = captureStderr(t, func() int { return Fail("x %d", 1) })
	if out != "claude-code: x 1\n" || code != 1 {
		t.Errorf("Fail = %q, %d", out, code)
	}
}

// ParseArgs must honor a flag after positionals and stop flag parsing at "--".
func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := FormatFlag(fs)
	pos, err := ParseArgs(fs, []string{"gemini", "--format", "json", "router", "--", "--format"})
	if err != nil || *f != JSON || !slices.Equal(pos, []string{"gemini", "router", "--format"}) {
		t.Fatalf("ParseArgs = %v, %v, format=%q", pos, err, *f)
	}
	if CheckFormat("xml") == nil || CheckFormat(Text) != nil {
		t.Error("Check accepts only text|json")
	}
}

// ParseArgs must never flag-parse anything after a literal "--", even a later "-h".
func TestParseDoubleDashIsAllPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	pos, err := ParseArgs(fs, []string{"--", "file.py", "-h"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if !slices.Equal(pos, []string{"file.py", "-h"}) {
		t.Fatalf("Parse positionals = %v, want [file.py -h]", pos)
	}
}

// ParseArgs must treat "--" as a non-bool flag's VALUE, not the terminator, when given bare ("--output --").
func TestParseDoubleDashAsFlagValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("output", "", "")
	pos, err := ParseArgs(fs, []string{"--output", "--", "x"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if *out != "--" {
		t.Fatalf("output = %q, want --", *out)
	}
	if !slices.Equal(pos, []string{"x"}) {
		t.Fatalf("Parse positionals = %v, want [x]", pos)
	}
}

// ParseArgs must still honor "--" as the terminator when the flag's value arrives via "=", not a separate token.
func TestParseEqualsFormThenDoubleDashTerminates(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("output", "", "")
	pos, err := ParseArgs(fs, []string{"--output=--", "x"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if *out != "--" {
		t.Fatalf("output = %q, want --", *out)
	}
	if !slices.Equal(pos, []string{"x"}) {
		t.Fatalf("Parse positionals = %v, want [x] (the = form never consumes a following terminator)", pos)
	}
}

// ParseArgs must still terminate on "--" right after a bool flag, since a bool flag never consumes the next token.
func TestParseBoolFlagThenDoubleDashTerminates(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "")
	pos, err := ParseArgs(fs, []string{"--verbose", "--", "-h"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if !*verbose {
		t.Fatal("verbose flag not set")
	}
	if !slices.Equal(pos, []string{"-h"}) {
		t.Fatalf("Parse positionals = %v, want [-h]", pos)
	}
}

// ParseArgs must still error on a genuinely unknown flag preceding "--": consumesValue only skips a token
// for a REGISTERED non-bool flag, so an unregistered one leaves the terminator scan untouched and
// fs.Parse still rejects the unknown flag on the head slice it was handed.
func TestParseUnknownFlagBeforeDoubleDashStillErrors(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	_, err := ParseArgs(fs, []string{"--nope", "--", "x"})
	if err == nil {
		t.Fatal("Parse err = nil, want an error for the unregistered flag")
	}
}

// Parse's value-skip must also apply to a short-form flag ("-o" not "--output") given without "=".
func TestParseShortFlagDoubleDashAsValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("o", "", "")
	pos, err := ParseArgs(fs, []string{"-o", "--", "x"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if *out != "--" {
		t.Fatalf("output = %q, want --", *out)
	}
	if !slices.Equal(pos, []string{"x"}) {
		t.Fatalf("Parse positionals = %v, want [x]", pos)
	}
}

func TestRemovedFlagHint(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // substring of the error; "" means nil
	}{
		{"provider", []string{"--provider", "x"}, "--profile"},
		{"provider=", []string{"--provider=x"}, "--profile"},
		{"env", []string{"--env", "qa"}, "--profile"},
		{"tier", []string{"--tier", "opus"}, "--model"},
		{"after other args", []string{"-p", "hi", "--tier=opus"}, "--tier"},
		{"profile valid", []string{"--profile", "x"}, ""},
		{"model valid", []string{"--model=opus"}, ""},
		{"after double dash", []string{"--", "--provider"}, ""},
		{"value lookalike", []string{"--profile", "--providers"}, ""},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RemovedFlagHint(tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("RemovedFlagHint(%q) = %v, want nil", tt.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RemovedFlagHint(%q) = %v, want containing %q", tt.args, err, tt.want)
			}
		})
	}
}
