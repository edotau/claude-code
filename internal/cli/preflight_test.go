package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPreflightProfileWithoutLiveFails(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdPreflight([]string{"--profile", "prod"}) })
	if code == 0 || !strings.Contains(stderr, "--profile requires --live") {
		t.Fatalf("code=%d stderr=%q, want non-zero with the --profile-requires---live message", code, stderr)
	}
}

func TestPreflightLiveUnknownProfileFailsWithoutSpawning(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return preflightLive(context.Background(), reg, "prod") })
	if code == 0 || !strings.Contains(stderr, `no pong ran for "prod"`) {
		t.Fatalf("code=%d stderr=%q, want non-zero no-pong failure", code, stderr)
	}
	if !strings.Contains(stdout, "unknown profile") {
		t.Fatalf("stdout=%q, want the skip row naming the unknown profile", stdout)
	}
}

func TestPreflightLiveCorruptPinsFails(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdPreflight([]string{"--live"}) })
	if code == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q, want failure when corrupt pins prevent every pong", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "vault error: agent pins: vault: agent-records is corrupt") {
		t.Fatalf("stdout=%q, want the corrupt agent-records diagnostic", stdout)
	}
	if !strings.Contains(stderr, "no pong ran: the pin check failed") {
		t.Fatalf("stderr=%q, want the no-pong failure", stderr)
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
