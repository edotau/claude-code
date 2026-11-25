package router

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPolicy(t *testing.T) {
	t.Run("modes", testPolicyModes)
	t.Run("aliases", testPolicyAliases)
	t.Run("sources", testPolicySources)
	t.Run("setting", testPolicySetting)
}

// Every persisted mode preserves both its spelling and the independent hop permissions.
func testPolicyModes(t *testing.T) {
	want := map[string]Policy{
		FallbackNone:   {Kind: PolicyNone},
		FallbackPin:    {Kind: PolicyAnyInFence},
		FallbackAgents: {Kind: PolicyAnyInFence, Agents: true},
		FallbackAll:    {Kind: PolicyAnyInFence, Agents: true, OffArm: true},
	}
	for _, mode := range FallbackModes {
		t.Run(mode, func(t *testing.T) {
			expected, ok := want[mode]
			if !ok {
				t.Fatalf("no expected policy for persisted mode %q", mode)
			}
			p, err := PolicyOf(mode, PolicyDefault)
			if err != nil || p != expected {
				t.Fatalf("PolicyOf(%q) = %+v, %v; want %+v", mode, p, err, expected)
			}
			if got := p.Mode(); got != mode {
				t.Errorf("Mode() = %q, want %q", got, mode)
			}
		})
	}
	if got := (Policy{Kind: PolicySameWorkspace}).Mode(); got != FallbackPin {
		t.Errorf("SameWorkspace Mode() = %q, want pin", got)
	}
}

func testPolicyAliases(t *testing.T) {
	for raw, mode := range map[string]string{
		"": FallbackNone, " \t": FallbackNone, "1": FallbackAll,
		"true": FallbackAll, "yes": FallbackAll, " ALL ": FallbackAll, " Pin ": FallbackPin,
	} {
		p, err := PolicyOf(raw, PolicyTyped)
		want, _ := PolicyOf(mode, PolicyTyped)
		if err != nil || p != want || p.Mode() != mode {
			t.Errorf("PolicyOf(%q) = %+v, %v; want %+v (mode %q)", raw, p, err, want, mode)
		}
	}
	for _, raw := range []string{"bogus", "false", "0"} {
		p, err := PolicyOf(raw, PolicyTyped)
		want := fmt.Sprintf("%s=%q unknown (%s)", EnvFallback, raw, strings.Join(FallbackModes, "|"))
		if err == nil || err.Error() != want || p != (Policy{}) {
			t.Errorf("PolicyOf(%q) = %+v, %v; want zero policy and %q", raw, p, err, want)
		}
	}
}

// A none request is still typed; env and vault defaults never become requests.
func testPolicySources(t *testing.T) {
	for _, src := range []PolicySource{PolicyDefault, PolicyEnv, PolicyOverlay, PolicyTyped} {
		for _, mode := range FallbackModes {
			p, err := PolicyOf(mode, src)
			if err != nil || p.Source != src || p.Mode() != mode {
				t.Fatalf("PolicyOf(%q, %v) = %+v, %v", mode, src, p, err)
			}
			if want := src == PolicyTyped || src == PolicyOverlay; p.Typed() != want {
				t.Errorf("PolicyOf(%q, %v).Typed() = %v, want %v", mode, src, p.Typed(), want)
			}
		}
	}
}

func testPolicySetting(t *testing.T) {
	vaulttest.Use(t)
	for _, tt := range []struct {
		name, overlay, env, vault, mode string
		source                          PolicySource
	}{
		{"default", "", "", "", "", PolicyDefault},
		{"blank", " \t", " \n", " \t", "", PolicyDefault},
		{"vault", "", "", " pin ", "pin", PolicyEnv},
		{"blank-env", "", " \t", "all", "all", PolicyEnv},
		{"env-over-vault", "", " agents ", "all", "agents", PolicyEnv},
		{"overlay-over-env", " none ", "agents", "all", "none", PolicyOverlay},
		{"blank-overlay", " \t", "none", "all", "none", PolicyEnv},
		{"raw-alias", " YES ", "", "", "YES", PolicyOverlay},
		{"invalid-overlay", "bogus", "all", "pin", "bogus", PolicyOverlay},
		{"invalid-env", "", "bogus", "pin", "bogus", PolicyEnv},
		{"invalid-vault", "", "", "bogus", "bogus", PolicyEnv},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvFallback, tt.env)
			if err := providers.SetCustomEnv(EnvFallback, tt.vault); err != nil {
				t.Fatal(err)
			}
			mode, src := FallbackSetting(context.Background(), tt.overlay)
			if mode != tt.mode || src != tt.source {
				t.Errorf("FallbackSetting() = %q, %v; want %q, %v", mode, src, tt.mode, tt.source)
			}
		})
	}
	t.Run("snapshot", testPolicySnapshot)
}

func testPolicySnapshot(t *testing.T) {
	t.Setenv(EnvFallback, "all")
	if err := providers.SetCustomEnv(EnvFallback, "pin"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		values proc.Values
		mode   string
	}{
		{proc.Values{EnvFallback: " agents "}, "agents"},
		{proc.Values{}, "pin"},
	} {
		ctx := proc.WithValues(context.Background(), tt.values)
		if mode, src := FallbackSetting(ctx, ""); mode != tt.mode || src != PolicyEnv {
			t.Errorf("snapshot %v: got %q, %v; want %q, PolicyEnv", tt.values, mode, src, tt.mode)
		}
	}
}

// The zero Policy resolves to the default mode, never to all or to a bare nothing.
func TestPolicyZeroResolves(t *testing.T) {
	want, _ := PolicyOf("", PolicyDefault)
	if got := (Policy{}).Resolved(); got != want {
		t.Fatalf("Resolved() = %+v, want %+v", got, want)
	}
	if got := ClaudeBackstop([]Candidate{{Agent: "claude"}, {Agent: "codex"}}, 1, Policy{}); got != -1 {
		t.Errorf("zero Policy ClaudeBackstop = %d, want -1 (default none)", got)
	}
}
