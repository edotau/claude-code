package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
)

func TestPreflightProfileWithoutLiveFails(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdPreflight([]string{"--profile", "prod"}) })
	if code == 0 || !strings.Contains(stderr, "--profile requires --live") {
		t.Fatalf("code=%d stderr=%q, want non-zero with the --profile-requires---live message", code, stderr)
	}
}

func TestPreflightDiscoverRejectsConflictingFlags(t *testing.T) {
	vaulttest.Use(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"live", []string{"--live"}},
		{"refresh", []string{"--refresh"}},
		{"profile", []string{"--profile", "prod"}},
		{"live-profile", []string{"--live", "--profile", "prod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--discover", "doctor-conflict-test-missing"}, tc.args...)
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdPreflight(args) })
			if code == 0 || !strings.Contains(stderr, "--discover cannot be combined with --live, --refresh or --profile") {
				t.Fatalf("code=%d stderr=%q, want discover conflict before discovery executes", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout=%q, want no execution output for conflicting flags", stdout)
			}
		})
	}
}

func TestPreflightSpecListsProfileFlag(t *testing.T) {
	var names []string
	for _, f := range preflightSpec().Flags {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "profile") {
		t.Fatalf("completion flags %v lack profile", names)
	}
}
