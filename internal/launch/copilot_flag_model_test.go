package launch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"github.com/edotau/claude-code/internal/agent"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An explicit --model (consumed by ParseArgs, so absent from args) selects the registry model over the default.
func TestCopilotRegistryExplicitModelWins(t *testing.T) {
	t.Cleanup(agent.WithCopilotOS("linux")) // the macOS refusal has its own test
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		eps := []map[string]any{}
		for _, n := range []string{"databricks-claude-sonnet-5-5", "databricks-claude-opus-5-5"} {
			eps = append(eps, map[string]any{"name": n, "task": "llm/v1/chat", "state": map[string]string{"ready": "READY"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": eps})
	}))
	t.Cleanup(srv.Close)
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport.(*http.Transport).Clone()
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server
	t.Cleanup(func() { http.DefaultTransport = old })

	row := `"dbx":{"preset":"databricks","host":"` + srv.URL + `","auth":{"type":"key","env":"TEST_GW_KEY"},` +
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}`
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+row+`}}}`)
	t.Setenv("TEST_GW_KEY", "dbx-key")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	oldLook := lookPath
	t.Cleanup(func() { lookPath = oldLook })
	lookPath = func(string) (string, error) { return "/bin/copilot", nil }

	argvModel := func(model string) string {
		plan, err := Vendor(context.Background(), "copilot", Options{Provider: "dbx", Model: model, PrintEnv: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range plan.Argv {
			if a == "--model" && i+1 < len(plan.Argv) {
				return plan.Argv[i+1]
			}
		}
		t.Fatalf("no --model in %v", plan.Argv)
		return ""
	}
	def, flagged := argvModel(""), argvModel("claude-opus-5-5")
	if def == flagged || !strings.HasSuffix(flagged, "/claude-opus-5-5") {
		t.Fatalf("default %q, flagged %q: --model was ignored", def, flagged)
	}
}
