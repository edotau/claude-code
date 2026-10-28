package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// dbxServing points a Databricks serving row's routes at srv (anthropic at /anthropic, openai/responses at /v1).
func dbxServing(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TEST_DBX_KEY", "dbx-key")
	body := fmt.Sprintf(`{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://h.cloud.databricks.com",
	  "base_url":%q,"routes":{"openai":%q,"responses":%q},"auth":{"type":"key","env":"TEST_DBX_KEY"}}}}}`,
		srv.URL+"/anthropic", srv.URL+"/v1", srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

// D2: a non-Claude model on a Databricks row rides the openai chat route, not the Claude-only /anthropic one.
func TestClaudeNoToolsNonClaudeModelUsesChatRoute(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"kimi-k3\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	dbxServing(t, srv)

	var out strings.Builder
	res, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "hi", Provider: "dbx", Model: "databricks-kimi-k3", NoTools: true}, &out)
	if err != nil || out.String() != "ok" || res.Provider != "dbx" {
		t.Fatalf("res = %+v err = %v out %q", res, err, out.String())
	}
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Errorf("paths = %v, want only the openai chat route", paths)
	}
}

// A Responses-only model has no one-shot wire: refused before any request, naming the model.
func TestClaudeNoToolsResponsesOnlyRefused(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	dbxServing(t, srv)

	_, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "hi", Provider: "dbx", Model: "databricks-gpt-5-5", NoTools: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "databricks-gpt-5-5") || hits != 0 {
		t.Errorf("err = %v, hits = %d", err, hits)
	}
}

// A Claude model on the same row still rides /anthropic.
func TestClaudeNoToolsClaudeModelUsesAnthropicRoute(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer srv.Close()
	dbxServing(t, srv)

	_, err := claudeRunner{}.Run(context.Background(), Request{Prompt: "hi", Provider: "dbx", Model: "databricks-claude-haiku-4-5", NoTools: true}, io.Discard)
	if err != nil || path != "/anthropic/v1/messages" {
		t.Errorf("err = %v, path = %q", err, path)
	}
}
