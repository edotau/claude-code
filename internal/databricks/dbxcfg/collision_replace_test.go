package dbxcfg

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func patCand(name string) Candidate {
	return Candidate{Name: name, Host: "https://example.cloud.databricks.com", Auth: providers.AuthPAT, HasSecret: true}
}

func TestPlanSkipsCollidingSecretNames(t *testing.T) {
	rows := Plan([]Candidate{patCand("a-b"), patCand("a_b")}, nil, Options{})
	if rows[0].Action != "add" {
		t.Errorf("first = %+v", rows[0])
	}
	if rows[1].Action != "skip" || rows[1].Reason == "" {
		t.Errorf("second should be skipped with a reason: %+v", rows[1])
	}
}

func TestPlanReplaceOfNoAuthTypeRowSkipsOnlyThatRow(t *testing.T) {
	existing := map[string]json.RawMessage{
		"keep": json.RawMessage(`{"preset":"databricks","host":"https://example.cloud.databricks.com","auth":{"type":"bearer"}}`),
	}
	rows := Plan([]Candidate{patCand("keep"), patCand("fresh")}, existing, Options{Replace: true})
	if rows[0].Action != "skip" || !strings.Contains(rows[0].Reason, "databricks login --profile keep --auth-type u2m") {
		t.Errorf("existing = %+v", rows[0])
	}
	if rows[1].Action != "add" {
		t.Errorf("fresh = %+v", rows[1])
	}
	top := map[string]json.RawMessage{}
	if err := Apply(top, rows); err != nil {
		t.Fatal(err)
	}
	if _, ok := Existing(top)["fresh"]; !ok {
		t.Error("fresh row not applied")
	}
}
