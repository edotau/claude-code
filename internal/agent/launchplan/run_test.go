package launchplan

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model", "--router": "removed",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := ParseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ParseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := ParseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}

// A launch refusal (*agent.UsageError, even wrapped) prints its message and exits 2, not the generic 1.
func TestRunPlanUsageErrorExitsTwo(t *testing.T) {
	err := fmt.Errorf("launch: %w", &UsageError{Msg: "not a harness profile"})
	if code := Run("claude", agent.Plan{}, agent.LaunchOptions{}, err); code != 2 {
		t.Fatalf("Run(UsageError) = %d, want 2", code)
	}
}
