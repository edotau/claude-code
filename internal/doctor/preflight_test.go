package doctor

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

func TestPreflightProfileWithoutLiveFails(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Preflight([]string{"--profile", "prod"}) })
	if code == 0 || !strings.Contains(stderr, "--profile requires --live") {
		t.Fatalf("code=%d stderr=%q, want non-zero with the --profile-requires---live message", code, stderr)
	}
}

func TestPreflightLiveUnknownProfileFailsWithoutSpawning(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return preflightLive(reg, "prod") })
	if code == 0 || !strings.Contains(stderr, `no pong ran for "prod"`) {
		t.Fatalf("code=%d stderr=%q, want non-zero no-pong failure", code, stderr)
	}
	if !strings.Contains(stdout, "unknown profile") {
		t.Fatalf("stdout=%q, want the skip row naming the unknown profile", stdout)
	}
}

func TestPreflightSpecNodeListsProfileFlag(t *testing.T) {
	var names []string
	for _, f := range PreflightSpecNode().Flags {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "profile") {
		t.Fatalf("completion flags %v lack profile", names)
	}
}
