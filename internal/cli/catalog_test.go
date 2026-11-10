package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// catalogListing is the fake workspace: Claude, a Responses-only GPT, open chat models, a cold and an embeddings row.
const catalogListing = `{"endpoints":[
{"name":"databricks-claude-sonnet-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-glm-5-2","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-cold-1","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
{"name":"databricks-embed-1","task":"llm/v1/embeddings","state":{"ready":"READY"}}]}`

// catalogEnv serves identity, listing and probes for profiles dev (serving) and gw (AI Gateway); returns paths hit.
func catalogEnv(t *testing.T) func() []string {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv("DATABRICKS_DEV_PAT", "synthetic-catalog-pat")
	t.Setenv("DATABRICKS_GW_PAT", "synthetic-catalog-pat")
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/2.0/preview/scim/v2/Me":
			_, _ = w.Write([]byte(`{"id":"catalog-subject","active":true}`))
		case "/api/2.0/serving-endpoints":
			_, _ = w.Write([]byte(catalogListing))
		default:
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	useTLSTransport(t, srv)
	for name, wire := range map[string]string{"dev": "", "gw": "ai-gateway"} {
		f := map[string]any{"preset": "databricks", "host": srv.URL, "auth": map[string]any{"type": "pat"}}
		if wire != "" {
			f["options"] = map[string]any{"wire": wire}
		}
		if err := providers.SetProviderFields(name, f); err != nil {
			t.Fatal(err)
		}
	}
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func runCatalog(t *testing.T, line string) (int, string, string) {
	t.Helper()
	args := strings.Fields(line)
	return hosttest.CaptureOutput(t, func() int { return cmdAgentVerb(args[0], args[1:]) })
}

// codex and copilot list, probe (flags after the verb) and sync their own endpoints; checks share one cache.
func TestCatalogListCheckSyncPerAgent(t *testing.T) {
	seen := catalogEnv(t)
	for _, c := range []struct {
		line      string
		want, not []string
	}{
		{"codex catalog check --profile dev --format json", []string{`"id": "gpt-5-5"`}, []string{"glm-5-2"}},
		{"copilot catalog check --profile dev", []string{"claude-sonnet-5-5", "glm-5-2"}, []string{"gpt-5-5"}},
		{"codex catalog list --profile dev", []string{"databricks-gpt-5-5  ok"}, []string{"glm-5-2"}},
		{"codex catalog sync --apply --profile dev", []string{"5 listed, 1 usable by codex", "sealed in vault"}, nil},
	} {
		out := mustCatalog(t, c.line)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output lacks %q:\n%s", c.line, w, out)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out, n) {
				t.Errorf("%s: output has %q, which this agent cannot use:\n%s", c.line, n, out)
			}
		}
	}
	if bare, list := mustCatalog(t, "codex catalog --profile dev"), mustCatalog(t, "codex catalog list --profile dev"); bare != list {
		t.Errorf("bare catalog = %q, want list's %q", bare, list)
	}
	for _, p := range seen() {
		if !strings.HasPrefix(p, "/serving-endpoints/") && !strings.HasPrefix(p, "/api/2.0/") {
			t.Errorf("catalog reached %s; want only /serving-endpoints routes and the listing/identity APIs", p)
		}
	}
}

// mustCatalog runs line and returns its stdout, failing the test on a non-zero exit.
func mustCatalog(t *testing.T, line string) string {
	t.Helper()
	code, out, errOut := runCatalog(t, line)
	if code != 0 {
		t.Fatalf("%s: code=%d stderr=%s", line, code, errOut)
	}
	return out
}

// A stray word or an opencode-only verb is a usage error (exit 2) for every agent, before any profile lookup.
func TestCatalogRefusesStrayArgumentsForEveryAgent(t *testing.T) {
	seen := catalogEnv(t)
	for _, line := range []string{"codex catalog bogus", "copilot catalog bogus --profile dev", "gemini catalog bogus",
		"opencode catalog bogus", "copilot catalog list extra --profile dev", "codex catalog sync extra --profile dev",
		"opencode catalog list extra", "codex catalog add glm-5-2", "copilot catalog --profile dev check"} {
		if code, _, errOut := runCatalog(t, line); code != 2 || !strings.Contains(errOut, "catalog") {
			t.Errorf("%s: code=%d stderr=%q; want a usage error", line, code, errOut)
		}
	}
	if n := len(seen()); n != 0 {
		t.Errorf("refused arguments reached the workspace %d times", n)
	}
}

// A profile on the AI Gateway wire is refused, bare or with a verb, and nothing is requested.
func TestCatalogRefusesAIGatewayProfile(t *testing.T) {
	seen := catalogEnv(t)
	for _, line := range []string{"codex catalog --profile gw", "copilot catalog check --profile gw",
		"opencode catalog check --all --profile gw", "codex catalog sync --profile gw --apply"} {
		code, _, errOut := runCatalog(t, line)
		if code != 1 || !strings.Contains(errOut, "profile gw uses the AI Gateway wire; catalog runs on /serving-endpoints only") {
			t.Errorf("%s: code=%d stderr=%q; want the AI Gateway refusal", line, code, errOut)
		}
	}
	if n := len(seen()); n != 0 {
		t.Errorf("a gateway profile reached the workspace %d times", n)
	}
}

// gemini has no Databricks route: its catalog verbs print one note and probe nothing.
func TestCatalogGeminiHasNoDatabricksRoute(t *testing.T) {
	seen := catalogEnv(t)
	code, out, _ := runCatalog(t, "gemini catalog check --all --profile dev")
	if code != 0 || out != "gemini catalog check: gemini has no Databricks route (it runs on its own login); nothing to check\n" {
		t.Errorf("gemini check: code=%d out=%q", code, out)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("gemini reached the workspace %d times", n)
	}
}
