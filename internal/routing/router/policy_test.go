package router

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
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
		route.FallbackNone:   {Kind: PolicyNone},
		route.FallbackPin:    {Kind: PolicyAnyInFence},
		route.FallbackAgents: {Kind: PolicyAnyInFence, Agents: true},
		route.FallbackAll:    {Kind: PolicyAnyInFence, Agents: true, OffArm: true},
	}
	for _, mode := range route.FallbackModes {
		t.Run(mode, func(t *testing.T) {
			expected, ok := want[mode]
			p, err := PolicyOf(mode, PolicyDefault)
			if !ok || err != nil || p != expected {
				t.Fatalf("PolicyOf(%q) = %+v, %v; want %+v", mode, p, err, expected)
			}
			if got := p.Mode(); got != mode {
				t.Errorf("Mode() = %q, want %q", got, mode)
			}
		})
	}
	if got := (Policy{Kind: PolicySameWorkspace}).Mode(); got != route.FallbackPin {
		t.Errorf("SameWorkspace Mode() = %q, want pin", got)
	}
}

func testPolicyAliases(t *testing.T) {
	for raw, mode := range map[string]string{
		"": route.FallbackNone, " \t": route.FallbackNone, "1": route.FallbackAll,
		"true": route.FallbackAll, "yes": route.FallbackAll, " ALL ": route.FallbackAll, " Pin ": route.FallbackPin,
	} {
		p, err := PolicyOf(raw, PolicyTyped)
		want, _ := PolicyOf(mode, PolicyTyped)
		if err != nil || p != want || p.Mode() != mode {
			t.Errorf("PolicyOf(%q) = %+v, %v; want %+v (mode %q)", raw, p, err, want, mode)
		}
	}
	for _, raw := range []string{"bogus", "false", "0"} {
		p, err := PolicyOf(raw, PolicyTyped)
		want := fmt.Sprintf("%s=%q unknown (%s)", EnvFallback, raw, strings.Join(route.FallbackModes, "|"))
		if err == nil || err.Error() != want || p != (Policy{}) {
			t.Errorf("PolicyOf(%q) = %+v, %v; want zero policy and %q", raw, p, err, want)
		}
	}
}

// A none request is still typed; env and vault defaults never become requests.
func testPolicySources(t *testing.T) {
	for _, src := range []PolicySource{PolicyDefault, PolicyEnv, PolicyOverlay, PolicyTyped} {
		for _, mode := range route.FallbackModes {
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

func TestEffectiveFallback(t *testing.T) {
	vaultEnv := map[string]string{EnvFallback: "agents"}
	for _, c := range []struct {
		overlay, proc string
		env           map[string]string
		mode, origin  string
	}{
		{"all", "pin", vaultEnv, "all", "overlay"},
		{"", "pin", vaultEnv, "pin", "process env"},
		{"", "", vaultEnv, "agents", "vault env"},
		{"", "", nil, "none", "default"},
	} {
		t.Setenv(EnvFallback, c.proc)
		if mode, origin := EffectiveFallback(context.Background(), c.overlay, c.env); mode != c.mode || origin != c.origin {
			t.Errorf("EffectiveFallback(%q, %q, %v) = %s, %s; want %s, %s", c.overlay, c.proc, c.env, mode, origin, c.mode, c.origin)
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

func TestBareFallback(t *testing.T) {
	for _, c := range []struct{ name, id, overlay, want string }{
		{"top level", "", "", route.FallbackAgents},
		{"dispatched child", "d-1", "", route.FallbackNone},
		{"confined", "", `{"provider":"vendorx","profile":"acct9"}`, route.FallbackNone},
		{"bad overlay", "", `{"provider":`, route.FallbackNone},
	} {
		t.Setenv(proc.DispatchIDEnv, c.id)
		t.Setenv(proc.DispatchDepthEnv, "")
		t.Setenv(route.OverlayVar, c.overlay)
		if got := BareFallback(context.Background()); got != c.want {
			t.Errorf("%s: BareFallback = %s, want %s", c.name, got, c.want)
		}
	}
}
