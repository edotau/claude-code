package databricks

import (
	"flag"
	"io"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// -p NAME selects the same profile as --profile NAME through bind() (config, rescue).
func TestProfileShorthandMatchesLongFlag(t *testing.T) {
	for _, args := range [][]string{{"-p", "bb"}, {"-p=bb"}, {"--profile", "bb"}} {
		var f cfgFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		bind(fs, &f, "harness", true)
		if err := fs.Parse(args); err != nil || f.profile != "bb" {
			t.Errorf("bind %v: profile=%q err=%v, want bb", args, f.profile, err)
		}
	}
}

// -p names the profile on check and on the bare launch verb; claude's print mode passes through as --print.
func TestPlanShorthandProfile(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	c, err := plan([]string{"check", "-p", "cc"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := plan([]string{"-p", "aa", "--print", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "cc" || len(c.Rest) != 0 {
		t.Errorf("check -p cc: provider %q rest %v", c.Provider, c.Rest)
	}
	if l.Provider != "aa" || len(l.Rest) != 2 || l.Rest[0] != "--print" {
		t.Errorf("launch -p aa: provider %q rest %v", l.Provider, l.Rest)
	}
}
