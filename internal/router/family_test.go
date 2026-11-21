package router

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestFamilyLatest(t *testing.T) {
	cases := []struct {
		name, family string
		ids          []string
		want         string
	}{
		{"6.2 beats 6.1 and 6", "sol", []string{"gpt-6-sol", "gpt-6.2-sol", "gpt-6.1-sol"}, "gpt-6.2-sol"},
		{"dashed Databricks ids", "sol", []string{"databricks-gpt-6-sol", "databricks-gpt-6-2-sol", "databricks-gpt-6-1-sol"}, "databricks-gpt-6-2-sol"},
		{"prefixed first-party id", "sol", []string{"openai/gpt-6.1-sol", "openai/gpt-6.2-sol"}, "openai/gpt-6.2-sol"},
		{"only 6.1 listed", "sol", []string{"gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-astra"}, "gpt-6.1-sol"},
		{"7 beats 6.9", "sol", []string{"gpt-6.9-sol", "gpt-7-sol"}, "gpt-7-sol"},
		{"6.10 beats 6.9", "sol", []string{"gpt-6.9-sol", "gpt-6.10-sol", "gpt-6.1-sol"}, "gpt-6.10-sol"},
		{"6-1 equals 6.1, first listed stays", "sol", []string{"gpt-6.1-sol", "databricks-gpt-6-1-sol"}, "gpt-6.1-sol"},
		{"no member", "luna", []string{"gpt-6-sol", "gpt-oss-120b"}, ""},
		{"no ids", "sol", nil, ""},
	}
	for _, c := range cases {
		if got := FamilyLatest(c.family, c.ids); got != c.want {
			t.Errorf("%s: FamilyLatest(%s) = %q, want %q", c.name, c.family, got, c.want)
		}
	}
}

func TestFamilyModelDatabricksListing(t *testing.T) {
	vaulttest.Use(t)
	p, now := dbxProv("dev"), time.Now()
	if got := FamilyModel("codex", "sol", p, nil, now); got != "databricks-gpt-6-1-sol" {
		t.Errorf("no listing = %q, want today's concrete id", got)
	}
	ready := map[string]string{"databricks-gpt-6-sol": "READY", "databricks-gpt-6-1-sol": "READY", "databricks-gpt-6-2-sol": "READY"}
	if err := serving.Save(listing("dev", ready)); err != nil {
		t.Fatal(err)
	}
	if got := FamilyModel("codex", "sol", p, nil, now); got != "databricks-gpt-6-2-sol" {
		t.Errorf("6, 6.1 and 6.2 listed = %q, want 6.2", got)
	}
	ready["databricks-gpt-6-2-sol"] = "NOT_READY"
	if err := serving.Save(listing("dev", ready)); err != nil {
		t.Fatal(err)
	}
	if got := FamilyModel("codex", "sol", p, nil, now); got != "databricks-gpt-6-1-sol" {
		t.Errorf("6.2 not ready = %q, want 6.1", got)
	}
	if got := FamilyModel("codex", "luna", p, nil, now); got != "databricks-gpt-6-luna" {
		t.Errorf("family with no listed member = %q, want today's concrete id", got)
	}
}

func TestFamilyModelFirstPartyList(t *testing.T) {
	gh := providersGitHub()
	got := FamilyModel("copilot", "sol", gh, []string{"gpt-6.1-sol", "gpt-6.2-sol"}, time.Now())
	if got != "gpt-6.2-sol" {
		t.Errorf("first-party list = %q, want gpt-6.2-sol", got)
	}
	if got := FamilyModel("copilot", "astra", gh, nil, time.Now()); got != "gpt-6-astra" {
		t.Errorf("no list = %q, want today's concrete id", got)
	}
}

func TestIsFamilyRejectsUnknownAndConcrete(t *testing.T) {
	for _, name := range []string{"sol", "Astra", " terra", "luna"} {
		if !providers.IsFamily(name) {
			t.Errorf("IsFamily(%q) = false", name)
		}
	}
	for _, name := range []string{"gpt-6.1-sol", "opus", "vega", ""} {
		if providers.IsFamily(name) {
			t.Errorf("IsFamily(%q) = true", name)
		}
	}
}

func TestDatabricksModelBucketFollowsFamily(t *testing.T) {
	vaulttest.Use(t)
	p := dbxProv("dev")
	ready := map[string]string{"databricks-gpt-6-1-sol": "READY", "databricks-gpt-6-2-sol": "READY"}
	if err := serving.Save(listing("dev", ready)); err != nil {
		t.Fatal(err)
	}
	// opus lists no claude here, so the first runnable member is the sol family's newest
	if got := DatabricksModel("codex", "opus", p); got != "databricks-gpt-6-2-sol" {
		t.Errorf("opus bucket = %q, want databricks-gpt-6-2-sol", got)
	}
}

func providersGitHub() *providers.Provider {
	return &providers.Provider{Name: "gh", Provider: "github"}
}

// A listed family id the catalog does not know yet is runnable by its family's wire, so codex picks it.
func TestFamilyModelPicksUnknownNewestForResponsesAgent(t *testing.T) {
	vaulttest.Use(t)
	if err := serving.Save(listing("dev", map[string]string{"databricks-gpt-7-sol": "READY"})); err != nil {
		t.Fatal(err)
	}
	if got := FamilyModel("codex", "sol", dbxProv("dev"), nil, time.Now()); got != "databricks-gpt-7-sol" {
		t.Errorf("codex sol = %q, want the listed databricks-gpt-7-sol", got)
	}
}
