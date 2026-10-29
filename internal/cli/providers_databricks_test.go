package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
)

const dbxCLIToken = "dapi-cli-secret-token"

func dbxCLIServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"|"+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"endpoints":[
{"name":"chat-a","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"embed-b","task":"llm/v1/embeddings","state":{"ready":"READY"}},
{"name":"chat-c","task":"llm/v1/chat","state":{"ready":"NOT_READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	return srv, &got
}

func dbxCLIProvider(host string) *providers.Provider {
	return &providers.Provider{Name: "qa", Preset: providers.PresetDatabricks, Host: host,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_CLI_TOKEN"}}
}

func TestListLiveDatabricksText(t *testing.T) {
	cliScratch(t)
	t.Setenv("DBX_CLI_TOKEN", dbxCLIToken)
	srv, got := dbxCLIServer(t)
	code, stdout, stderr := captureOutput(t, func() int {
		return listLive(&providers.Registry{}, dbxCLIProvider(srv.URL), outfmt.Text)
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 rows:\n%s", stdout)
	}
	wantFields := []string{"NAME TASK READY", "chat-a llm/v1/chat READY", "embed-b llm/v1/embeddings READY", "chat-c llm/v1/chat NOT_READY"}
	for i, w := range wantFields {
		if f := strings.Join(strings.Fields(lines[i]), " "); f != w {
			t.Errorf("line %d = %q, want %q", i, f, w)
		}
	}
	if len(*got) != 1 || (*got)[0] != "/api/2.0/serving-endpoints|Bearer "+dbxCLIToken {
		t.Errorf("requests = %v", *got)
	}
	if strings.Contains(stdout, dbxCLIToken) || strings.Contains(stderr, dbxCLIToken) {
		t.Fatal("output leaked the bearer token")
	}
}

func TestListLiveDatabricksJSON(t *testing.T) {
	cliScratch(t)
	t.Setenv("DBX_CLI_TOKEN", dbxCLIToken)
	srv, _ := dbxCLIServer(t)
	code, stdout, stderr := captureOutput(t, func() int {
		return listLive(&providers.Registry{}, dbxCLIProvider(srv.URL), outfmt.JSON)
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(raw) != 3 || raw[0]["id"] != "chat-a" || raw[0]["ready"] != "READY" || raw[1]["task"] != "llm/v1/embeddings" ||
		raw[2]["id"] != "chat-c" || raw[2]["ready"] != "NOT_READY" {
		t.Errorf("rows = %v", raw)
	}
	if strings.Contains(stdout, dbxCLIToken) || strings.Contains(stderr, dbxCLIToken) {
		t.Fatal("output leaked the bearer token")
	}
}

// Negative control: a plain provider prints the ID/Model table and its JSON rows carry no task/ready keys.
func TestListLiveNonDatabricksHasNoTaskReady(t *testing.T) {
	cliScratch(t)
	srv := fakeModelsServer(t, []string{"model-a"})
	_, text, _ := captureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), outfmt.Text) })
	if !strings.HasPrefix(text, "ID") || strings.Contains(text, "TASK") {
		t.Errorf("text = %q, want the ID/Model table", text)
	}
	_, js, _ := captureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), outfmt.JSON) })
	if strings.Contains(js, `"task"`) || strings.Contains(js, `"ready"`) {
		t.Errorf("json = %s, want no task/ready keys", js)
	}
}

// An unverifiable pin (endpoints unreachable) must not stop later pins from being validated.
func TestValidateCopilotPinsChecksPinsAfterUnverifiableOne(t *testing.T) {
	useVaultTest(t)
	t.Setenv("DBX_CLI_TOKEN", dbxCLIToken)
	srv, _ := dbxCLIServer(t)
	for name, host := range map[string]string{"dbx-down": "https://127.0.0.1:1", "dbx-up": srv.URL} {
		err := providers.SetProviderFields(name, map[string]any{
			"protocol": "anthropic", "preset": "databricks", "host": host,
			"auth": map[string]any{"type": "key", "env": "DBX_CLI_TOKEN"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := validateCopilotPins("", []string{"opus=dbx-down:chat-a", "sonnet=dbx-up:bogus-id"})
	if err == nil || !strings.Contains(err.Error(), "bogus-id") {
		t.Fatalf("err = %v, want the bogus-id pin rejected after the unreachable one", err)
	}
}
