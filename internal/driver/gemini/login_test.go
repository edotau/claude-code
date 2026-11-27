package gemini

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// An unreadable login marker is an error, not "not signed in", and the probe reports it.
func TestGeminiLoginUnreadableMarkerIsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if ok, err := geminiLoginProbe(); ok || err == nil {
		t.Fatalf("geminiLoginProbe() = (%v, %v), want (false, error)", ok, err)
	}
	_, _, err := geminiLogin("gemini-3.8-flash", nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("geminiLogin err = %v, want an unreadable-marker error", err)
	}
}

func TestGeminiRouteCapsFallbackParity(t *testing.T) {
	for _, tc := range []struct {
		name, pin       string
		marker, profile bool
	}{
		{name: "absent", profile: true},
		{name: "no profile", marker: true},
		{name: "registry model", marker: true, profile: true},
		{name: "model pin", pin: "gemini-3.8-flash", marker: true, profile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agyFixture(t, nil)
			t.Setenv("GEMINI_MODEL", tc.pin)
			if !tc.marker {
				if err := os.Remove(AgyLoginMarker(os.Getenv("HOME"))); err != nil {
					t.Fatal(err)
				}
			}
			p := &providers.Provider{Name: "fixture", Provider: "google", Auth: providers.Auth{Login: "gemini"},
				Models: map[string]string{models.Opus: "gemini-4-hydrogen"}}
			reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
			if tc.profile {
				reg.Providers[p.Name] = p
			}
			got, note, ok := agent.SchemaFor("gemini").Eng().FallbackTarget(t.Context(), reg)
			if !tc.marker || !tc.profile {
				if ok || got.Provider != nil || got.Model != "" || note != "" {
					t.Fatalf("unexpected fallback: %+v, %q, %v", got, note, ok)
				}
				return
			}
			want := agent.PinnedTarget(t.Context(), "gemini", providers.Target{Provider: p, Model: p.Models[models.Opus]})
			if !ok || got.Provider != p || got.Model != want.Model || note != "gemini (Google login via agy)" {
				t.Fatalf("fallback = %+v, %q, %v; want %+v", got, note, ok, want)
			}
		})
	}
}

func TestGeminiFallbackWarnsWithoutProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	marker := AgyLoginMarker(os.Getenv("HOME"))
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(marker, marker); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = out
	t.Cleanup(func() { os.Stderr = old; _ = out.Close() })
	got, note, ok := agent.SchemaFor("gemini").Eng().FallbackTarget(t.Context(), &providers.Registry{})
	if ok || got.Provider != nil || note != "" {
		t.Fatalf("unreadable marker produced fallback: %+v, %q, %v", got, note, ok)
	}
	body, err := os.ReadFile(out.Name())
	if err != nil || !strings.Contains(string(body), "gemini: agy login marker unreadable, Google-login fallback dropped:") {
		t.Fatalf("diagnostic = %q, %v", body, err)
	}
}

func TestGeminiRouteCapsKeepsGatewayPolicy(t *testing.T) {
	agyFixture(t, nil)
	d := agent.SchemaFor("gemini").Eng()
	p := &providers.Provider{Name: "fixture", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "key"}}
	if d.FollowsPin(p) || d.NoRoute(t.Context(), nil, providers.Target{Provider: p}) == nil {
		t.Fatal("an unpinned gateway must not silently run")
	}
	vaulttest.SetRecord(t, "gemini", "profile", p.Name)
	if !d.FollowsPin(p) {
		t.Fatal("an explicitly pinned gateway must remain usable")
	}
	for _, p := range []*providers.Provider{nil, {}, {Provider: "google"}, {Auth: providers.Auth{Login: "gemini"}}} {
		want := p != nil && p.Auth.Login == "gemini"
		if d.LoginServes(p) != want || !d.FollowsPin(p) || d.NoRoute(t.Context(), nil, providers.Target{Provider: p}) != nil {
			t.Errorf("login/pin/refusal policy changed for %+v", p)
		}
	}
	want := agent.RouteInfo{Agent: "gemini", Route: agent.RouteNone, Reason: "no fallback chain defined for gemini"}
	if got := d.DescribeRoute(t.Context(), nil); got != want || d.LaunchEnv() != nil {
		t.Errorf("default route/env changed: %+v, %q", got, d.LaunchEnv())
	}
}
