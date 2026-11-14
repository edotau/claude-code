package catalog

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fixtureServing is a serving workspace's listing: Claude, a Responses-only GPT, open chat models and non-candidates.
var fixtureServing = map[string]providers.ModelInfo{
	"databricks-claude-sonnet-5-5": {ID: "databricks-claude-sonnet-5-5", Task: "llm/v1/chat", Ready: "READY"},
	"databricks-gpt-5-5":           {ID: "databricks-gpt-5-5", Task: "llm/v1/chat", Ready: "READY"},
	"databricks-glm-5-2":           {ID: "databricks-glm-5-2", Task: "llm/v1/chat", Ready: "READY"},
	"databricks-cold-1":            {ID: "databricks-cold-1", Task: "llm/v1/chat", Ready: "NOT_READY"},
	"databricks-embed-1":           {ID: "databricks-embed-1", Task: "llm/v1/embeddings", Ready: "READY"},
}

// fixtureProvider is a Databricks serving row with every serving route (anthropic, openai, responses).
func fixtureProvider() *providers.Provider {
	host := "https://dbc-1.cloud.databricks.com"
	return &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Protocol: providers.ProtocolAnthropic,
		Host: host, BaseURL: host + "/serving-endpoints/anthropic", ModelPrefix: "databricks-",
		Routes: map[string]string{providers.ProtocolOpenAI: host + "/serving-endpoints",
			providers.ProtocolResponses: host + "/serving-endpoints"}}
}

// Each agent sees only the READY chat endpoints whose wire is one of its dialects (the roster's Dialects).
func TestUsableFiltersByAgentDialects(t *testing.T) {
	vaulttest.Use(t) // ServesResponses reads the vault menu; the built-in one keeps gpt-5-5 on Responses
	for _, tc := range []struct {
		agent    string
		dialects []string
		want     []string
	}{
		{"codex", []string{providers.ProtocolResponses}, []string{"databricks-gpt-5-5"}},
		{"copilot", []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
			[]string{"databricks-claude-sonnet-5-5", "databricks-glm-5-2"}},
		{"opencode", []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
			[]string{"databricks-claude-sonnet-5-5", "databricks-glm-5-2"}},
		{"anthropic-wire", []string{providers.ProtocolAnthropic}, []string{"databricks-claude-sonnet-5-5"}},
		{"gemini", nil, nil},
	} {
		var got []string
		for _, info := range UsableIn(fixtureProvider(), slices.Collect(maps.Values(fixtureServing)), tc.dialects) {
			got = append(got, info.ID)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s (%v): UsableIn = %v, want %v", tc.agent, tc.dialects, got, tc.want)
		}
	}
}

// A profile on the AI Gateway wire is refused before any listing or probe, whichever agent asks.
func TestProfileRefusesAIGatewayWire(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.EnvSessionProfile, "")
	err := providers.SetProviderFields("gw", map[string]any{"preset": "databricks", "host": "https://dbc-1.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"}, "options": map[string]any{"wire": "ai-gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Profile(context.Background(), "codex", "gw")
	if p != nil || !errors.Is(err, ErrGateway) ||
		err.Error() != "profile gw uses the AI Gateway wire; catalog runs on /serving-endpoints only" {
		t.Fatalf("Profile(gw) = %v, %v; want the AI Gateway refusal", p, err)
	}
}

// A run snapshot on ctx picks the catalog profile; an empty snapshot suppresses the ambient session profile.
func TestProfileFollowsTheRunSnapshot(t *testing.T) {
	vaulttest.Use(t)
	err := providers.SetProviderFields("gw", map[string]any{"preset": "databricks", "host": "https://dbc-1.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"}, "options": map[string]any{"wire": "ai-gateway"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(providers.EnvSessionProfile, "gw")
	if p, err := Profile(proc.WithValues(context.Background(), proc.Values{}), "codex", ""); p != nil || err != nil {
		t.Errorf("empty snapshot: Profile = %v, %v; want none (the ambient gw leaked)", p, err)
	}
	t.Setenv(providers.EnvSessionProfile, "")
	snap := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: "gw"})
	if _, err := Profile(snap, "codex", ""); !errors.Is(err, ErrGateway) {
		t.Errorf("snapshot gw: err = %v, want the AI Gateway refusal", err)
	}
}
