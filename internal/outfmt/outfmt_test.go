package outfmt

import (
	"flag"
	"slices"
	"testing"
)

// Parse must honor a flag after positionals and stop flag parsing at "--".
func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := Flag(fs)
	pos, err := Parse(fs, []string{"gemini", "--format", "json", "router", "--", "--format"})
	if err != nil || *f != JSON || !slices.Equal(pos, []string{"gemini", "router", "--format"}) {
		t.Fatalf("Parse = %v, %v, format=%q", pos, err, *f)
	}
	if Check("xml") == nil || Check(Text) != nil {
		t.Error("Check accepts only text|json")
	}
}

// Parse must never flag-parse anything after a literal "--", even a later "-h".
func TestParseDoubleDashIsAllPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	pos, err := Parse(fs, []string{"--", "file.py", "-h"})
	if err != nil {
		t.Fatalf("Parse err = %v, want nil", err)
	}
	if !slices.Equal(pos, []string{"file.py", "-h"}) {
		t.Fatalf("Parse positionals = %v, want [file.py -h]", pos)
	}
}
