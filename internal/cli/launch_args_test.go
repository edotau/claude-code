package cli

import (
	"slices"
	"strings"
	"testing"
)

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model", "--router": "removed",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := parseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("parseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := parseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}
