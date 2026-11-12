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
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
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
	ambiguous := copilotRegistry{Profile: "dev", Models: []copilotRegistryModel{
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
	route, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: &providers.Provider{Name: "fixture"}})
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
	route, err := prepareCopilotRegistry(ctx, providers.Target{Provider: &providers.Provider{Name: "fixture"}, Model: "claude-sonnet-5-5"})
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
	oldDiscover, oldRouter, oldMint := discoverModels, agent.EnsureRouter, mintSession
	t.Cleanup(func() { discoverModels, agent.EnsureRouter, mintSession = oldDiscover, oldRouter, oldMint })
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		return []CopilotEndpoint{{ID: "claude-sonnet-5-5"}}, nil
	}
	agent.EnsureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	mintSession = func(context.Context, []string, int) (string, error) { return "fixture-session", nil }
}

// A keyed Databricks row runs on the registry except inside a databricks:<p> route on macOS, which takes routed BYOK.
func TestArmRegistryEligibility(t *testing.T) {
	dbx := providers.Target{Provider: &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: "key", Env: "DBX_CLI_TOKEN"}}}
	for _, c := range []struct {
		goos, route string
		want        bool
	}{{"darwin", "databricks:dbx", false}, {"darwin", "", true}, {"linux", "databricks:dbx", true}} {
		setGOOS(t, c.goos)
		overlay := ""
		if c.route != "" {
			overlay = `{"provider":"databricks","profile":"dbx"}`
		}
		t.Setenv(agentconfig.OverlayVar, overlay)
		if got := copilotRegistryEligible(false, dbx); got != c.want {
			t.Errorf("%s route %q: registry eligible = %v, want %v", c.goos, c.route, got, c.want)
		}
	}
}
