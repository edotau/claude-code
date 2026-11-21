package serving

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// WarmServed is newer-wins: an older or empty listing leaves the file alone, a newer one replaces it.
func TestWarmServedNewerWins(t *testing.T) {
	vaulttest.Use(t)
	base := time.Now().UTC().Truncate(time.Second)
	if err := WarmServed("dev", []providers.ModelInfo{{ID: "m-a", Ready: "READY"}}, base); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		infos []providers.ModelInfo
		at    time.Time
		// the one model id on disk afterwards
		want string
	}{
		{"older than the file", []providers.ModelInfo{{ID: "m-old"}}, base.Add(-time.Minute), "m-a"},
		{"empty infos", nil, base.Add(time.Minute), "m-a"},
		{"newer", []providers.ModelInfo{{ID: "m-new", Task: "llm/v1/chat", Ready: "READY"}}, base.Add(time.Minute), "m-new"},
	} {
		if err := WarmServed("dev", c.infos, c.at); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, ok := serving.Load("dev")
		if !ok || len(got.Models) != 1 || got.Models[0].ID != c.want {
			t.Errorf("%s: on disk %+v (ok=%v), want only %s", c.name, got.Models, ok, c.want)
		}
	}
}

// Given a warmed listing, Then api_types and limits survive the cache and a model without them reads as unknown.
func TestWarmServedKeepsFoundationFacts(t *testing.T) {
	vaulttest.Use(t)
	infos := []providers.ModelInfo{{ID: "databricks-gpt-5-4", Task: "llm/v1/chat", Ready: "READY", ModelClass: "gpt",
		APITypes: []string{"openai/v1/responses"}, ContextToken: 400000, OutputToken: 128000}, {ID: "databricks-plain"}}
	if err := WarmServed("facts", infos, time.Now()); err != nil {
		t.Fatal(err)
	}
	s, ok := serving.Load("facts")
	p := &providers.Provider{Name: "facts", Preset: providers.PresetDatabricks, ModelPrefix: "databricks-"}
	m, found := s.Find(p, "gpt-5-4")
	if !ok || !found || m.APITypes[0] != "openai/v1/responses" || m.Context != 400000 || m.Output != 128000 {
		t.Fatalf("Find = %+v, %v, %v", m, found, ok)
	}
	if plain, _ := s.Find(p, "plain"); plain.APITypes != nil || plain.Context != 0 {
		t.Errorf("plain = %+v, want unknown facts", plain)
	}
}
