package copilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

// buildDevRegistry runs the real builder over eps on a non-Databricks row, where every endpoint rides anthropic.
func buildDevRegistry(ids ...string) copilotRegistry {
	eps := make([]CopilotEndpoint, len(ids))
	for i, id := range ids {
		eps[i] = CopilotEndpoint{ID: id}
	}
	return copilotBuildRegistry("dev", &providers.Provider{Name: "dev"}, eps, "http://127.0.0.1:1/")
}

// Two endpoints with one canonical id become one model on one provider; either spelling selects it.
func TestBuildRegistryDedupesCanonicalIDs(t *testing.T) {
	r := buildDevRegistry("databricks-claude-sonnet-5-5", "claude-sonnet-5-5", "databricks-gpt-5-5")
	if len(r.Models) != 2 || len(r.Providers) != 1 || r.Models[0].WireModel != "databricks-claude-sonnet-5-5" {
		t.Fatalf("registry = %+v, want 2 models (first spelling kept) on 1 provider", r)
	}
	for _, s := range []string{"claude-sonnet-5-5", "databricks-claude-sonnet-5-5", "sonnet", "dev-anthropic/claude-sonnet-5-5"} {
		if ref, err := r.Select(modelChoice{Requested: []string{s}}); err != nil || ref != "dev-anthropic/claude-sonnet-5-5" {
			t.Errorf("Select(%q) = %q, %v; want the one sonnet model", s, ref, err)
		}
	}
}

// Each selection failure names its sentinel; ambiguity needs two models one query matches.
func TestRegistrySelectErrors(t *testing.T) {
	r := buildDevRegistry("databricks-claude-sonnet-5-5", "databricks-gpt-5-5")
	ambiguous := copilotRegistry{Profile: "dev", Models: []RegistryModel{
		{ID: "gpt-5-5", Provider: "a", WireModel: "w"}, {ID: "gpt-5-6", Provider: "b", WireModel: "w"},
	}}
	for _, c := range []struct {
		name string
		r    copilotRegistry
		c    modelChoice
		want error
	}{
		{"repeated", r, modelChoice{Requested: []string{"sonnet", "gpt-5-5"}}, errRepeatedSelection},
		{"unknown", r, modelChoice{Requested: []string{"kimi-k3"}}, errUnknownModel},
		{"pin gone", r, modelChoice{Pin: "kimi-k3"}, errSavedModelGone},
		{"empty", copilotRegistry{}, modelChoice{}, errNoModels},
		{"ambiguous", ambiguous, modelChoice{Requested: []string{"w"}}, errAmbiguousModel},
	} {
		if _, err := c.r.Select(c.c); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

// An empty target model lets the default apply instead of failing as the unknown model "".
func TestPrepareCopilotRegistryEmptyModelUsesDefault(t *testing.T) {
	hosttest.PinScratch(t)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubSettingsRegistry(t)
	route, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: fixtureKeyedRow()})
	if err != nil || route == nil || route.ref == "" {
		t.Fatalf("prepareCopilotRegistry(empty model) = %+v, %v; want the default model selected", route, err)
	}
}

// With no flag and no pin, the profile's default model (minus [1m]) is the choice; a flag or pin still wins.
func TestCopilotChoiceProfileDefault(t *testing.T) {
	hosttest.PinScratch(t)
	t.Setenv("COPILOT_MODEL", "")
	if got := copilotChoice(nil, "claude-sonnet-5-5[1m]").Requested; !slices.Equal(got, []string{"claude-sonnet-5-5"}) {
		t.Errorf("no flag/pin: Requested = %v, want the profile default without [1m]", got)
	}
	if got := copilotChoice([]string{"gpt-5-5"}, "claude-sonnet-5-5").Requested; !slices.Equal(got, []string{"gpt-5-5"}) {
		t.Errorf("flag: Requested = %v, want the flag only", got)
	}
	t.Setenv("COPILOT_MODEL", "kimi-k3")
	if c := copilotChoice(nil, "claude-sonnet-5-5"); len(c.Requested) != 0 || c.Pin != "kimi-k3" {
		t.Errorf("pin: choice = %+v, want the pin and no profile default", c)
	}
}

func TestSDKRegistryWarnsAndDropsUnreadableSettings(t *testing.T) {
	hosttest.PinScratch(t)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(os.Getenv("COPILOT_HOME"), "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(copilotRegistryHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(copilotRegistryHome(), "settings.json")
	regressionWrite(t, stale, `{"defaultPermissionMode":"allow-all"}`)
	stubSettingsRegistry(t)
	logPath := filepath.Join(t.TempDir(), "dispatch.log")
	sink := wire.NewLogSink(logPath)
	t.Cleanup(sink.Close)
	ctx := wire.WithLog(context.Background(), sink)
	route, err := prepareCopilotRegistry(ctx, providers.Target{Provider: fixtureKeyedRow(), Model: "claude-sonnet-5-5"})
	if err != nil || route == nil {
		t.Fatalf("prepareCopilotRegistry = %v, %v", route, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale private settings still usable: %v", err)
	}
	sink.Close()
	raw, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(raw), "note: copilot: settings.json not carried over") {
		t.Fatalf("SDK warning log = %q, %v; want settings read warning", raw, err)
	}
}

func stubSettingsRegistry(t *testing.T) {
	t.Helper()
	oldDiscover, oldRouter, oldMint := discoverModels, agent.EnsureRouter, agent.MintSession
	agent.ResetSessionTokens()
	t.Cleanup(func() {
		discoverModels, agent.EnsureRouter, agent.MintSession = oldDiscover, oldRouter, oldMint
		agent.ResetSessionTokens()
	})
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		return []CopilotEndpoint{{ID: "claude-sonnet-5-5"}}, nil
	}
	agent.EnsureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	agent.MintSession = func(context.Context, []string, int) (string, error) { return "fixture-session", nil }
}

// The registry router base is the session base minus /p/<profile>; its token is the one EndpointURL minted.
func TestCopilotRouterBaseAndKey(t *testing.T) {
	stubSettingsRegistry(t)
	base, key, err := copilotRouter(context.Background(), fixtureKeyedRow())
	if err != nil || base != "http://127.0.0.1:1" || key != "fixture-session" {
		t.Fatalf("copilotRouter = %q, %q, %v; want the bare router base and the minted token", base, key, err)
	}
}

// A direct unit never reaches the registry: its row has no router to sit behind.
func TestCopilotRouterRefusesDirectUnit(t *testing.T) {
	stubSettingsRegistry(t)
	ctx := agent.WithDirect(context.Background())
	if _, _, err := copilotRouter(ctx, fixtureKeyedRow()); err == nil {
		t.Fatal("copilotRouter on a direct unit succeeded; want the registry to refuse it")
	}
}

func fixtureKeyedRow() *providers.Provider {
	return &providers.Provider{Name: "fixture", Auth: providers.Auth{Type: "key", Env: "FIXTURE_TOKEN"}}
}

// A keyed Databricks row runs on the registry except on macOS, which takes routed BYOK.
func TestArmRegistryEligibility(t *testing.T) {
	dbx := providers.Target{Provider: &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: "key", Env: "DBX_CLI_TOKEN"}}}
	for _, c := range []struct {
		goos, route string
		want        bool
	}{{"darwin", "databricks:dbx", false}, {"darwin", "", false}, {"linux", "databricks:dbx", true}} {
		setGOOS(t, c.goos)
		overlay := ""
		if c.route != "" {
			overlay = `{"provider":"databricks","profile":"dbx"}`
		}
		t.Setenv(route.OverlayVar, overlay)
		if got := copilotRegistryEligible(false, dbx); got != c.want {
			t.Errorf("%s route %q: registry eligible = %v, want %v", c.goos, c.route, got, c.want)
		}
	}
}
