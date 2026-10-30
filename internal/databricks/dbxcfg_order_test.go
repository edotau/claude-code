package databricks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

const orderCfg = `[prod]
host = https://prod.cloud.databricks.com
token = dapi1

[dev]
host = https://dev.cloud.databricks.com
token = dapi2

[qa]
host = https://qa.cloud.databricks.com
token = dapi3
`

func TestImportFollowsSectionOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(orderCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	if want := []string{"prod", "dev", "qa"}; !slices.Equal(names, want) {
		t.Fatalf("Candidates = %v, want section order %v", names, want)
	}
	top := map[string]json.RawMessage{}
	if err := Apply(top, ImportPlan(cs, Existing(top), ImportOptions{})); err != nil {
		t.Fatal(err)
	}
	if got, want := providers.ObjectKeys(top["providers"]), []string{"prod", "dev", "qa"}; !slices.Equal(got, want) {
		t.Errorf("Apply order = %v, want %v (not sorted)", got, want)
	}
}

func TestApplyKeepsExistingRowsInPlaceAndAppendsNew(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(p, []byte(orderCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Candidates(p)
	if err != nil {
		t.Fatal(err)
	}
	ex, _ := providers.MarshalOrdered([]string{"qa", "legacy"}, map[string]json.RawMessage{
		"qa":     json.RawMessage(`{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}`),
		"legacy": json.RawMessage(`{"preset":"databricks","host":"https://l.cloud.databricks.com"}`),
	})
	top := map[string]json.RawMessage{"providers": ex}
	if err := Apply(top, ImportPlan(cs, Existing(top), ImportOptions{Replace: true})); err != nil {
		t.Fatal(err)
	}
	if got, want := providers.ObjectKeys(top["providers"]), []string{"qa", "legacy", "prod", "dev"}; !slices.Equal(got, want) {
		t.Errorf("Apply order = %v, want %v", got, want)
	}
}
