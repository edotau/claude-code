package copilot

import (
	"errors"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// buildDevRegistry runs the real builder over eps on a non-Databricks row, where every endpoint rides anthropic.
func buildDevRegistry(ids ...string) CopilotRegistry {
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
		if ref, err := r.Select(CopilotChoice{Requested: []string{s}}); err != nil || ref != "dev-anthropic/claude-sonnet-5-5" {
			t.Errorf("Select(%q) = %q, %v; want the one sonnet model", s, ref, err)
		}
	}
}

// Each selection failure names its sentinel; ambiguity needs two models one query matches.
func TestRegistrySelectErrors(t *testing.T) {
	r := buildDevRegistry("databricks-claude-sonnet-5-5", "databricks-gpt-5-5")
	ambiguous := CopilotRegistry{Profile: "dev", Models: []CopilotRegistryModel{
		{ID: "gpt-5-5", Provider: "a", WireModel: "w"}, {ID: "gpt-5-6", Provider: "b", WireModel: "w"},
	}}
	for _, c := range []struct {
		name string
		r    CopilotRegistry
		c    CopilotChoice
		want error
	}{
		{"repeated", r, CopilotChoice{Requested: []string{"sonnet", "gpt-5-5"}}, ErrRepeatedSelection},
		{"unknown", r, CopilotChoice{Requested: []string{"kimi-k3"}}, ErrUnknownModel},
		{"pin gone", r, CopilotChoice{Pin: "kimi-k3"}, ErrSavedModelGone},
		{"empty", CopilotRegistry{}, CopilotChoice{}, ErrNoModels},
		{"ambiguous", ambiguous, CopilotChoice{Requested: []string{"w"}}, ErrAmbiguousModel},
	} {
		if _, err := c.r.Select(c.c); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}
