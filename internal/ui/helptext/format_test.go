package helptext

import (
	"flag"
	"slices"
	"testing"
)

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
