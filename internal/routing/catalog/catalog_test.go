package catalog

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/databricks/serving"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

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

// A run snapshot on ctx picks the catalog profile; an empty snapshot suppresses the ambient session profile.
func TestProfileFollowsTheRunSnapshot(t *testing.T) {
	vaulttest.Use(t)
	err := providers.SetProviderFields("gw", map[string]any{"preset": "databricks", "host": "https://dbc-1.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(providers.EnvSessionProfile, "gw")
	p, err := Profile(proc.WithValues(context.Background(), proc.Values{}), "codex", "")
	if err != nil || (p != nil && p.Name == "gw") {
		t.Errorf("empty snapshot: Profile = %v, %v; want the own login or none (the ambient gw leaked)", p, err)
	}
	t.Setenv(providers.EnvSessionProfile, "")
	snap := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: "gw"})
	if p, err := Profile(snap, "codex", ""); err != nil || p == nil || p.Name != "gw" {
		t.Errorf("snapshot gw: Profile = %v, %v; want gw", p, err)
	}
}

func TestIDCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"databricks-glm-5-2": "glm-5-2", "glm-5-2": "glm-5-2", "databricks-gpt-6-1-sol": "gpt-6-1-sol", "kimi-k3": "kimi-k3",
	} {
		if got := ID(in); got != want {
			t.Errorf("ID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The probe target's wire is the prefixed served name first, else the bare one; neither marks it missing.
func TestTargetsServedLookup(t *testing.T) {
	served := map[string]providers.ModelInfo{"databricks-glm-5-2": {}, "kimi-k3": {}, "acme-kimi-k3": {}, "both": {},
		"databricks-both": {}}
	for _, tc := range []struct{ prefix, id, wire string }{
		{"databricks-", "glm-5-2", "databricks-glm-5-2"},
		{"databricks-", "kimi-k3", "kimi-k3"},
		{"databricks-", "both", "databricks-both"},
		{"acme-", "kimi-k3", "acme-kimi-k3"},
		{"databricks-", "absent-1", ""},
	} {
		got := targets([]string{tc.id}, false, served, &providers.Provider{ModelPrefix: tc.prefix}, nil)[0]
		if got.Wire != tc.wire || got.Missing != (tc.wire == "") {
			t.Errorf("%s%s: wire %q missing %v, want wire %q", tc.prefix, tc.id, got.Wire, got.Missing, tc.wire)
		}
	}
}

func TestCheckSortOrder(t *testing.T) {
	rs := []serving.CheckResult{
		{ID: "kimi-k3", Status: "ok", LatencyMS: 5}, {ID: "glm-5-2", Status: "fail", LatencyMS: 1},
		{ID: "glm-5-3", Status: "ok", LatencyMS: 900}, {ID: "glm-5-1", Status: "ok", LatencyMS: 100},
		{ID: "claude-x", Status: "ok", LatencyMS: 50},
	}
	sortCheckResults(rs)
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	if want := "claude-x glm-5-1 glm-5-3 glm-5-2 kimi-k3"; strings.Join(ids, " ") != want {
		t.Errorf("order = %v, want %s", ids, want)
	}
}

func TestCheckFamily(t *testing.T) {
	for in, want := range map[string]string{"claude-opus-5-5": "claude", "databricks-glm-5-2": "glm", "gpt-oss-120b": "gpt", "inkling": "inkling"} {
		if got := checkFamily(in); got != want {
			t.Errorf("checkFamily(%q) = %q, want %q", in, got, want)
		}
	}
}
