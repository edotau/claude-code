package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const servingBody = `{"endpoints":[
  {"name":"databricks-claude-sonnet","task":"llm/v1/chat","state":{"ready":"READY"}},
  {"name":"embed-small","task":"llm/v1/embeddings","state":{"ready":"READY"}},
  {"name":"chat-down","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
  {"name":"gpt-x","task":"llm/v1/chat","state":{"ready":"READY"}}]}`

func TestCopilotModelsServingRows(t *testing.T) {
	vaulttest.Use(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/2.0/serving-endpoints" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(servingBody))
	}))
	t.Cleanup(srv.Close)
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
	t.Setenv("DBX_COPILOT_MODELS_TOKEN", "tok")

	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dbx": {Name: "dbx", Preset: providers.PresetDatabricks, Host: srv.URL,
			Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_COPILOT_MODELS_TOKEN"}},
		"other": {Name: "other"},
	}}

	// Databricks row: embedding and NOT_READY rows dropped, order kept, prefixed id matches the unprefixed menu id.
	got, err := CopilotModelSource(context.Background(), reg, "dbx")
	if err != nil {
		t.Fatalf("CopilotModelSource(dbx): %v", err)
	}
	if !got.Databricks || got.Source != "dbx serving endpoints" || got.Profile != "dbx" {
		t.Errorf("catalog = %+v, want Databricks dbx serving endpoints", got)
	}
	if len(got.Rows) != 2 || got.Rows[0].ID != "databricks-claude-sonnet" || got.Rows[1].ID != "gpt-x" {
		t.Fatalf("rows = %+v, want [databricks-claude-sonnet gpt-x]", got.Rows)
	}
	menu := []providers.OpencodeModel{{ID: "claude-sonnet"}}
	rows := copilotreg.ServingChatRows([]providers.ModelInfo{
		{ID: "databricks-claude-sonnet", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "gpt-x", Task: "llm/v1/chat", Ready: "READY"},
	}, menu)
	if len(rows) != 2 || !rows[0].Tested || rows[1].Tested {
		t.Errorf("Tested = %+v, want [true false] (prefix stripped vs menu)", rows)
	}

	// Non-Databricks row: GitHub catalog, no network call, no rows.
	gh, err := CopilotModelSource(context.Background(), reg, "other")
	if err != nil {
		t.Fatalf("CopilotModelSource(other): %v", err)
	}
	if gh.Databricks || gh.Source != "GitHub Copilot catalog" || len(gh.Rows) != 0 {
		t.Errorf("catalog = %+v, want GitHub Copilot catalog, no rows", gh)
	}
}
