package catalog

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/databricks/serving"
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
