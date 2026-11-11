package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain registers the roster rows this package cannot link in and fails the binary when opencode's is missing.
func TestMain(m *testing.M) {
	agent.RegisterRowForTest(agent.Schema{Name: agentid.Codex, CLI: agent.CLI{Binary: "codex",
		Dialects: []string{providers.ProtocolResponses}}})
	agent.RegisterRowForTest(agent.Schema{Name: agentid.Copilot, CLI: agent.CLI{Binary: "copilot",
		Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI}}})
	agent.RegisterRowForTest(agent.Schema{Name: agentid.OpenCode, CLI: agent.CLI{Binary: "opencode",
		Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI}}})
	if len(agent.SchemaFor(agentid.OpenCode).CLI.Dialects) == 0 {
		fmt.Fprintln(os.Stderr, "discovery tests: the opencode roster fixture row has no dialects")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

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

// shape marks blank and READY rows ready, names the wire each id is served on and flags Responses-only ids.
func TestShape(t *testing.T) {
	workspace(t)
	p, err := catalog.Profile(agentid.Codex, "dev")
	if err != nil {
		t.Fatal(err)
	}
	got := shape(p, []providers.ModelInfo{
		{ID: "databricks-claude-sonnet-5-5", Name: "Sonnet", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-glm-5-2", Task: "llm/v1/chat"},
		{ID: "databricks-cold-1", Task: "llm/v1/chat", Ready: "NOT_READY"},
		{ID: "databricks-gpt-5-5", Task: "llm/v1/chat", Ready: "READY"},
	})
	want := []Model{
		{ID: "databricks-claude-sonnet-5-5", Label: "Sonnet", Ready: true, Task: "llm/v1/chat", Dialect: providers.ProtocolAnthropic},
		{ID: "databricks-glm-5-2", Ready: true, Task: "llm/v1/chat", Dialect: providers.ProtocolOpenAI},
		{ID: "databricks-cold-1", Task: "llm/v1/chat", Dialect: providers.ProtocolOpenAI},
		{ID: "databricks-gpt-5-5", Ready: true, Task: "llm/v1/chat", Dialect: providers.ProtocolResponses, Responses: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("shape =\n%+v\nwant\n%+v", got, want)
	}
}

// gemini is listed by its own CLI: the result says so and the workspace is never asked, even on a named profile.
func TestModelsGeminiIsVendorListed(t *testing.T) {
	hits := workspace(t)
	res, err := Models(context.Background(), Query{Agent: agentid.Gemini, Profile: "dev", Mode: Refresh})
	if err != nil || res.Source != SourceVendor || len(res.Models) != 0 || len(res.Warnings) != 1 {
		t.Errorf("Models(gemini) = %+v, %v; want the vendor source, no models and one warning", res, err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("gemini discovery made %d HTTP request(s); want none", n)
	}
}

// An agent outside the roster is refused by name.
func TestModelsUnknownAgent(t *testing.T) {
	workspace(t)
	_, err := Models(context.Background(), Query{Agent: "nope"})
	if err == nil || err.Error() != `discovery: unknown agent "nope"` {
		t.Errorf("Models(nope) error = %v; want the unknown agent error", err)
	}
}

// opencode with no profile names the flag on Recall and collapses to one line otherwise; AI Gateway is always named.
func TestModelsOpencodeProfileErrors(t *testing.T) {
	workspace(t)
	for mode, want := range map[Mode]string{
		Recall:   "no profile pinned for opencode; pass --profile",
		Verified: "cannot resolve profile; check the provider configuration",
	} {
		_, err := Models(context.Background(), Query{Agent: agentid.OpenCode, Mode: mode})
		if err == nil || err.Error() != want {
			t.Errorf("mode %d, no profile: error = %v, want %q", mode, err, want)
		}
		_, err = Models(context.Background(), Query{Agent: agentid.OpenCode, Profile: "gw", Mode: mode})
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
		res, err := Models(context.Background(), Query{Agent: tc.agent, Profile: "dev", Mode: Verified})
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
