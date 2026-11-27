package agent

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// A hub cell with no opencode Hub linked into the binary is a dispatch error, not a nil-pointer panic.
func TestHubDispatchWithoutHubIsAnError(t *testing.T) {
	orig := hub
	hub = nil
	t.Cleanup(func() { hub = orig })
	if _, _, err := hubDispatchFor(&providers.Registry{}, "opencode-go/kimi"); !errors.Is(err, errNoHub) {
		t.Fatalf("hubDispatchFor err = %v, want errNoHub", err)
	}
	if _, err := hubStart(&providers.Registry{}, "claude", ""); !errors.Is(err, errNoHub) {
		t.Fatalf("hubStart err = %v, want errNoHub", err)
	}
}

func TestBaseRouteDefaults(t *testing.T) {
	b := NewBaseEngine("codex")
	p := &providers.Provider{Auth: providers.Auth{Login: "codex"}}
	target := providers.Target{Provider: p, Model: "model"}
	if !b.FollowsPin(p) || !b.LoginServes(p) || b.LoginServes(nil) || b.LaunchEnv() != nil {
		t.Fatal("legacy pin/login/env defaults changed")
	}
	if got, note, ok, err := b.PreRoute(t.Context(), nil, false); got.Provider != nil || note != "" || ok || err != nil {
		t.Fatalf("PreRoute = %+v, %q, %v, %v", got, note, ok, err)
	}
	for _, gate := range []func(context.Context, RunOptions, *providers.Registry, providers.Target) (providers.Target, string, error){b.Gate, b.GateExplicit} {
		if got, note, err := gate(t.Context(), RunOptions{}, nil, target); !reflect.DeepEqual(got, target) || note != "" || err != nil {
			t.Fatalf("gate = %+v, %q, %v", got, note, err)
		}
	}
	if got, note, ok := b.FallbackTarget(t.Context(), nil); got.Provider != nil || note != "" || ok {
		t.Fatalf("fallback = %+v, %q, %v", got, note, ok)
	}
	if err := b.NoRoute(t.Context(), nil, target); err != nil {
		t.Fatal(err)
	}
}

func TestBaseRouteCaps(t *testing.T) {
	caps := RouteCaps{Login: "shared", LoginProviders: []string{"github"}, IgnorePin: true, LaunchEnv: []string{"X_ROUTE=1"}}
	b := NewBaseEngine("test").WithRouteCaps(caps)
	caps.LoginProviders[0], caps.LaunchEnv[0] = "changed", "X_ROUTE=2"
	b.LaunchEnv()[0] = "X_ROUTE=3"
	for _, tc := range []struct {
		provider, login string
		want            bool
	}{{"other", "shared", true}, {"github", "copilot", true}, {"other", "test", false}, {"other", "", false}} {
		p := &providers.Provider{Provider: tc.provider, Auth: providers.Auth{Login: tc.login}}
		if b.LoginServes(p) != tc.want || b.FollowsPin(p) {
			t.Errorf("policy for %+v: serves=%v follows=%v", tc, b.LoginServes(p), b.FollowsPin(p))
		}
	}
	if !slices.Equal(b.LaunchEnv(), []string{"X_ROUTE=1"}) {
		t.Fatal("launch env aliases caller storage")
	}
}

func TestBaseRouteLoginFallback(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "test-login", Provider: "openai", Auth: providers.Auth{Login: "shared"}, Models: map[string]string{"opus": "catalog"}}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{p.Name: p}}
	for _, tc := range []struct {
		name, own, pin, want string
		checkErr             error
		missing              bool
	}{{name: "catalog", want: "catalog"}, {name: "own", own: "local", want: "local"},
		{name: "pin", own: "local", pin: "pinned", want: "pinned"},
		{name: "no login", checkErr: errors.New("signed out")}, {name: "no row", missing: true}} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			b := NewBaseEngine("codex").WithRouteCaps(RouteCaps{Login: "shared", FallbackLogin: LoginFallback{
				Provider: "openai", Note: "login", OwnModel: func() string { return tc.own },
				Check: func() error { checks++; return tc.checkErr },
			}})
			r := reg
			if tc.missing {
				r = &providers.Registry{}
			}
			ctx := proc.WithValues(t.Context(), proc.Values{"CODEX_MODEL": tc.pin})
			got, note, ok := b.FallbackTarget(ctx, r)
			if ok != (tc.want != "") || got.Model != tc.want {
				t.Fatalf("fallback = %+v, %q, %v; want %q", got, note, ok, tc.want)
			}
			if ok && (got.Provider != p || note != "login") {
				t.Fatalf("fallback lost provider or note: %+v, %q", got, note)
			}
			if tc.missing && checks != 0 {
				t.Fatal("probed login without a registry row")
			}
		})
	}
}

func TestBaseRouteResolvedFallback(t *testing.T) {
	p := &providers.Provider{Name: "test-fallback"}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{p.Name: p}}
	ri := RouteInfo{Agent: "test", Provider: p.Name, Model: "model", Reason: "fallback", OK: true}
	b := NewBaseEngine("test").WithRouteCaps(RouteCaps{RequireFallback: true,
		ResolveFallback: func(context.Context, *providers.Registry) RouteInfo { return ri },
		FallbackLogin:   LoginFallback{Provider: "unused", Check: func() error { t.Fatal("resolver must win"); return nil }},
	})
	if got, note, ok := b.FallbackTarget(t.Context(), reg); got.Provider != p || got.Model != ri.Model || note != ri.Reason || !ok {
		t.Fatalf("fallback = %+v, %q, %v", got, note, ok)
	}
	if b.DescribeRoute(t.Context(), reg) != ri || b.NoRoute(t.Context(), reg, providers.Target{}) != nil {
		t.Fatal("description and no-route policy disagree")
	}
	ri.OK = false
	if _, _, ok := b.FallbackTarget(t.Context(), reg); ok {
		t.Fatal("unavailable route resolved")
	}
	if err := b.NoRoute(t.Context(), reg, providers.Target{}); err == nil || err.Error() != ri.Reason {
		t.Fatalf("NoRoute = %v", err)
	}
}

func TestBaseRouteLaunchEnvDispatch(t *testing.T) {
	b := NewBaseEngine("codex").WithRouteCaps(RouteCaps{LaunchEnv: []string{"X_ROUTE=default", "X_BASE=1"}})
	orig := drivers["codex"]
	drivers["codex"] = b
	t.Cleanup(func() { drivers["codex"] = orig })
	st := Settings{Record: vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"X_RECORD=1"}, Args: []string{"--extra"}}}}
	for _, headless := range []bool{false, true} {
		st.Headless = headless
		env, argv, _, cleanup, err := b.RenderSettings(t.Context(), st, []string{"X_ROUTE=resolved"}, nil)
		cleanup()
		if err != nil || !slices.Contains(env, "X_ROUTE=resolved") || slices.Contains(env, "X_ROUTE=default") {
			t.Fatalf("route env precedence: %v, %v", env, err)
		}
		if !slices.Contains(env, "X_BASE=1") || !slices.Contains(env, "X_RECORD=1") || slices.Contains(argv, "--extra") == headless {
			t.Fatalf("launch settings: env=%v argv=%v headless=%v", env, argv, headless)
		}
	}
}
