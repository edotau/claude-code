package catalog

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

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
