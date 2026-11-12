package configure

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// listing is the fake workspace: Claude, a Responses-only GPT, an open chat model, a cold and an embeddings row.
const listing = `{"endpoints":[
{"name":"databricks-claude-sonnet-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-glm-5-2","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-cold-1","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
{"name":"databricks-embed-1","task":"llm/v1/embeddings","state":{"ready":"READY"}}]}`

// workspace serves identity and listing for profiles dev (serving) and gw (AI Gateway); hits counts every request.
func workspace(t *testing.T) (hits *atomic.Int32) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv("DATABRICKS_DEV_PAT", "synthetic-discovery-pat")
	t.Setenv("DATABRICKS_GW_PAT", "synthetic-discovery-pat")
	hits = new(atomic.Int32)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/api/2.0/preview/scim/v2/Me" {
			_, _ = w.Write([]byte(`{"id":"discovery-subject","active":true}`))
			return
		}
		_, _ = w.Write([]byte(listing))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	for name, wire := range map[string]string{"dev": "", "gw": "ai-gateway"} {
		f := map[string]any{"preset": "databricks", "host": srv.URL, "auth": map[string]any{"type": "pat"}}
		if wire != "" {
			f["options"] = map[string]any{"wire": wire}
		}
		if err := providers.SetProviderFields(name, f); err != nil {
			t.Fatal(err)
		}
	}
	return hits
}

// An agent outside the roster is refused by name.
func TestModelsUnknownAgent(t *testing.T) {
	workspace(t)
	_, err := DiscoverModels(context.Background(), ModelQuery{Agent: "nope"})
	if err == nil || err.Error() != `discovery: unknown agent "nope"` {
		t.Errorf("Models(nope) error = %v; want the unknown agent error", err)
	}
}

// opencode with no profile names the flag on Recall and collapses to one line otherwise; AI Gateway is always named.
func TestModelsOpencodeProfileErrors(t *testing.T) {
	workspace(t)
	for mode, want := range map[ModelMode]string{
		RecallModels:   "no profile pinned for opencode; pass --profile",
		VerifiedModels: "cannot resolve profile; check the provider configuration",
	} {
		_, err := DiscoverModels(context.Background(), ModelQuery{Agent: agentid.OpenCode, Mode: mode})
		if err == nil || err.Error() != want {
			t.Errorf("mode %d, no profile: error = %v, want %q", mode, err, want)
		}
		_, err = DiscoverModels(context.Background(), ModelQuery{Agent: agentid.OpenCode, Profile: "gw", Mode: mode})
		if !errors.Is(err, catalog.ErrGateway) {
			t.Errorf("mode %d, gw profile: error = %v, want catalog.ErrGateway", mode, err)
		}
	}
}

// One workspace listing, one query shape: each agent gets the ids its dialects reach and the same raw rows.
func TestModelsFiltersOneListingPerAgent(t *testing.T) {
	workspace(t)
	chat := []string{"databricks-claude-sonnet-5-5", "databricks-glm-5-2"}
	for _, tc := range []struct {
		agent string
		want  []string
	}{
		{agentid.Codex, []string{"databricks-gpt-5-5"}},
		{agentid.Copilot, chat},
		{agentid.OpenCode, chat},
	} {
		res, err := DiscoverModels(context.Background(), ModelQuery{Agent: tc.agent, Profile: "dev", Mode: VerifiedModels})
		if err != nil {
			t.Fatalf("%s: %v", tc.agent, err)
		}
		var got []string
		for _, m := range res.Models {
			got = append(got, m.ID)
		}
		if !slices.Equal(got, tc.want) || len(res.Raw) != 5 || res.Profile != "dev" || res.Snapshot == nil {
			t.Errorf("%s: models = %v (raw %d, profile %q, snapshot %v); want %v over 5 raw rows on dev",
				tc.agent, got, len(res.Raw), res.Profile, res.Snapshot != nil, tc.want)
		}
	}
}
