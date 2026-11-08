package providerscmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeModelsServer serves the OpenAI-shaped {"data":[{"id":...}]} catalog listLive's non-copilot dispatch GETs.
func fakeModelsServer(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	type entry struct {
		ID string `json:"id"`
	}
	data := make([]entry, len(ids))
	for i, id := range ids {
		data[i] = entry{ID: id}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct {
			Data []entry `json:"data"`
		}{data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeProvider is a minimal, unauthenticated openai-dialect provider pointed at a local httptest server.
func fakeProvider(baseURL string) *providers.Provider {
	return &providers.Provider{Name: "fake", Protocol: providers.ProtocolOpenAI, BaseURL: baseURL, Auth: providers.Auth{Type: providers.AuthNone}}
}

func TestCmdModelsRejectsPositionalArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		arg  string
	}{
		{"plain args", []string{"foo", "bar"}, "foo"},
		{"pin with trailing arg", []string{"--pin", "all=x", "foo"}, "foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cliScratch(t)
			code, stdout, stderr := captureOutput(t, func() int { return Models(tc.args) })
			if code != 2 {
				t.Fatalf("exit = %d, want 2; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if want := "claude-code: models: unexpected argument \"" + tc.arg + "\"\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if err := vault.View(func(tx *vault.Tx) error {
				if pins := tx.All(vault.Pins); len(pins) != 0 {
					t.Errorf("vault pins = %v, want no pin mutation", pins)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// listLive's copilot dispatch (dispatched via the provider name, listLiveCopilot no longer exists standalone)
// reports the copilot CLI as not found (never a panic) when it is absent from bin/.
func TestListLiveCopilotReportsMissingCLI(t *testing.T) {
	cliScratch(t)
	reg := &providers.Registry{}
	p := &providers.Provider{Name: "personal", Provider: "github"}
	code, _, stderr := captureOutput(t, func() int { return listLive(reg, p, helptext.Text) })
	if code == 0 {
		t.Fatal("listLive: want a non-zero exit when the copilot CLI is missing")
	}
	if !strings.Contains(stderr, "copilot CLI not found") {
		t.Errorf("stderr = %q, want it to name the missing CLI", stderr)
	}
}

// listLive's non-copilot dispatch renders a fake provider's catalog as JSON rows, each carrying a use_case.
func TestListLiveJSONFormatsFakeCatalog(t *testing.T) {
	cliScratch(t)
	srv := fakeModelsServer(t, []string{"model-a", "model-b"})
	p := fakeProvider(srv.URL)
	reg := &providers.Registry{}
	code, stdout, _ := captureOutput(t, func() int { return listLive(reg, p, helptext.JSON) })
	if code != 0 {
		t.Fatalf("listLive exit = %d", code)
	}
	var rows []struct {
		ID      string `json:"id"`
		UseCase string `json:"use_case"`
	}
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout not a JSON array: %v\n%s", err, stdout)
	}
	if len(rows) != 2 || rows[0].ID != "model-a" || rows[1].ID != "model-b" {
		t.Errorf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.UseCase == "" {
			t.Errorf("row %+v: use_case must never be empty (models.UseCase always returns a label)", r)
		}
	}
}

// listLive's subscription (passthrough) dispatch falls back to the registry's configured models on a 401,
// and never prints the OAuth bearer token it tried — in the note, the table, or anywhere else.
func TestListLiveSubscriptionFallbackNeverPrintsToken(t *testing.T) {
	cliScratch(t)
	const secret = "sk-setup-token-super-secret-value"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	p := &providers.Provider{
		Name:     "sub",
		Protocol: providers.ProtocolAnthropic,
		BaseURL:  srv.URL,
		Auth:     providers.Auth{Type: providers.AuthCLI},
		Models:   map[string]string{"opus": "claude-opus-5-5"},
	}
	reg := &providers.Registry{}
	code, stdout, stderr := captureOutput(t, func() int { return listLive(reg, p, helptext.Text) })
	if code != 0 {
		t.Fatalf("listLive exit = %d, stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("output leaked the OAuth token\nstdout=%q\nstderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "live catalog needs an API key") {
		t.Errorf("stderr = %q, want the fallback note", stderr)
	}
	if !strings.Contains(stdout, "claude-opus-5-5") {
		t.Errorf("stdout = %q, want the registry's configured model in the fallback table", stdout)
	}
}

const dbxCLIToken = "dapi-cli-secret-token"

// providersDbxListing is the fake workspace listing (not dbxListing: these tests pin its exact three rows).
const providersDbxListing = `{"endpoints":[
{"name":"chat-a","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"embed-b","task":"llm/v1/embeddings","state":{"ready":"READY"}},
{"name":"chat-c","task":"llm/v1/chat","state":{"ready":"NOT_READY"}}]}`

func dbxCLIServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"|"+r.Header.Get("Authorization"))
		if r.URL.Path == "/ai-gateway/anthropic/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"system.ai.claude-opus-5","display_name":"Claude Opus 5"}],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(providersDbxListing))
	}))
	useTLSTransport(t, srv)
	return srv, &got
}

func dbxCLIProvider(host string) *providers.Provider {
	return &providers.Provider{Name: "qa", Preset: providers.PresetDatabricks, Host: host,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_CLI_TOKEN"}}
}

func assertNoDbxTokenLeak(t *testing.T, stdout, stderr string) {
	t.Helper()
	if strings.Contains(stdout, dbxCLIToken) || strings.Contains(stderr, dbxCLIToken) {
		t.Fatal("output leaked the bearer token")
	}
}

func TestListLiveDatabricksText(t *testing.T) {
	cliScratch(t)
	t.Setenv("DBX_CLI_TOKEN", dbxCLIToken)
	srv, got := dbxCLIServer(t)
	code, stdout, stderr := captureOutput(t, func() int {
		return listLive(&providers.Registry{}, dbxCLIProvider(srv.URL), helptext.Text)
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 8 || lines[5] != "AI GATEWAY (anthropic)" || !strings.Contains(lines[7], "Claude Opus 5") {
		t.Fatalf("want header + 3 rows + gateway section:\n%s", stdout)
	}
	wantFields := []string{"NAME TASK READY", "chat-a llm/v1/chat READY", "embed-b llm/v1/embeddings READY", "chat-c llm/v1/chat NOT_READY"}
	for i, w := range wantFields {
		if f := strings.Join(strings.Fields(lines[i]), " "); f != w {
			t.Errorf("line %d = %q, want %q", i, f, w)
		}
	}
	if len(*got) != 2 || (*got)[0] != "/api/2.0/serving-endpoints|Bearer "+dbxCLIToken {
		t.Errorf("requests = %v", *got)
	}
	assertNoDbxTokenLeak(t, stdout, stderr)
}

func assertDbxJSONRows(t *testing.T, raw []map[string]any) {
	t.Helper()
	if len(raw) != 4 || raw[3]["gateway"] != true || raw[0]["gateway"] != nil {
		t.Errorf("rows = %v", raw)
		return
	}
	if raw[0]["id"] != "chat-a" || raw[0]["ready"] != "READY" || raw[1]["task"] != "llm/v1/embeddings" ||
		raw[2]["id"] != "chat-c" || raw[2]["ready"] != "NOT_READY" {
		t.Errorf("rows = %v", raw)
	}
}

func TestListLiveDatabricksJSON(t *testing.T) {
	cliScratch(t)
	t.Setenv("DBX_CLI_TOKEN", dbxCLIToken)
	srv, _ := dbxCLIServer(t)
	code, stdout, stderr := captureOutput(t, func() int {
		return listLive(&providers.Registry{}, dbxCLIProvider(srv.URL), helptext.JSON)
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	assertDbxJSONRows(t, raw)
	assertNoDbxTokenLeak(t, stdout, stderr)
}

// Negative control: a plain provider prints the ID/Model table and its JSON rows carry no task/ready keys.
func TestListLiveNonDatabricksHasNoTaskReady(t *testing.T) {
	cliScratch(t)
	srv := fakeModelsServer(t, []string{"model-a"})
	_, text, _ := captureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), helptext.Text) })
	if !strings.HasPrefix(text, "ID") || strings.Contains(text, "TASK") {
		t.Errorf("text = %q, want the ID/Model table", text)
	}
	_, js, _ := captureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), helptext.JSON) })
	if strings.Contains(js, `"task"`) || strings.Contains(js, `"ready"`) {
		t.Errorf("json = %s, want no task/ready keys", js)
	}
}

// qaProfile seeds a hermetic vault with a Databricks row "qa" at host, served-aware routing set to on.
func qaProfile(t *testing.T, host string, on bool) {
	t.Helper()
	cliScratch(t)
	vaulttest.Use(t)
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, host))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-qa")
}

// statusServer answers every request with code and counts them.
func statusServer(t *testing.T, code int) (*httptest.Server, *int) {
	t.Helper()
	n := new(int)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*n++
		w.WriteHeader(code)
	}))
	useTLSTransport(t, srv)
	return srv, n
}

// switch onto a Databricks profile with no cache lists the workspace once and caches it.
func TestSwitchWarmsMissingServedCache(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, true)
	code, stdout, stderr := captureOutput(t, func() int { return Switch([]string{"qa"}) })
	if code != 0 || !strings.Contains(stdout, "profile pinned: qa") {
		t.Fatalf("exit %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if len(*got) == 0 {
		t.Fatal("switch made no listing request")
	}
	if s, ok := router.LoadServed("qa"); !ok || len(s.Models) != 3 {
		t.Fatalf("cache after switch = %+v ok=%v, want the 3 listed endpoints", s, ok)
	}
}

// a listing failure prints the warning and still exits 0 with the pin written.
func TestSwitchRefreshFailurePrintsAndExitsZero(t *testing.T) {
	srv, n := statusServer(t, http.StatusInternalServerError)
	qaProfile(t, srv.URL, true)
	code, stdout, stderr := captureOutput(t, func() int { return Switch([]string{"qa"}) })
	if code != 0 || !strings.Contains(stdout, "profile pinned: qa") || !strings.Contains(stdout, "listing refresh failed") {
		t.Fatalf("exit %d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if *n == 0 {
		t.Error("the refresh never reached the server")
	}
}

// --dry-run previews the pin and touches neither the vault nor the network.
func TestSwitchDryRunMakesNoRequests(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, true)
	if code, _, stderr := captureOutput(t, func() int { return Switch([]string{"qa", "--dry-run"}) }); code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr)
	}
	if len(*got) != 0 {
		t.Fatalf("dry-run made requests: %v", *got)
	}
	if _, ok := router.LoadServed("qa"); ok {
		t.Error("dry-run wrote a cache")
	}
}

// switch off (the default): the pin lands and no request is made.
func TestSwitchWithSwitchOffMakesNoRequests(t *testing.T) {
	srv, got := dbxCLIServer(t)
	qaProfile(t, srv.URL, false)
	if code, out, _ := captureOutput(t, func() int { return Switch([]string{"qa"}) }); code != 0 || !strings.Contains(out, "pinned") {
		t.Fatalf("exit %d out=%q", code, out)
	}
	if len(*got) != 0 {
		t.Fatalf("switch off still made requests: %v", *got)
	}
}

// switch moves claude and each agent that is on, and leaves an off agent off (dry-run says so).
func TestSwitchKeepsOffAgentsOff(t *testing.T) {
	srv, _ := dbxCLIServer(t)
	qaProfile(t, srv.URL, false)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "chatgpt"}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := captureOutput(t, func() int { return Switch([]string{"qa", "--dry-run"}) })
	if !strings.Contains(out, "codex.profile: chatgpt → qa") || !strings.Contains(out, "copilot: off (kept)") {
		t.Fatalf("dry-run = %q", out)
	}
	if code, _, stderr := captureOutput(t, func() int { return Switch([]string{"qa"}) }); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr)
	}
	recs := vault.AgentRecords(agent.RosterNames())
	if recs["claude"].Profile != "qa" || recs["codex"].Profile != "qa" || recs["copilot"].Profile != "" {
		t.Fatalf("profiles claude=%q codex=%q copilot=%q, want qa qa and copilot off",
			recs["claude"].Profile, recs["codex"].Profile, recs["copilot"].Profile)
	}
}

func repoLastProfileCached() bool {
	_, err := os.Stat(filepath.Join(paths.StateDir(), "last-profile.json"))
	return err == nil
}

func assertSwitchDryRunWritesNothing(t *testing.T) {
	t.Helper()
	if code, _, stderr := captureOutput(t, func() int { return Switch([]string{"qa", "--dry-run"}) }); code != 0 || !repoLastProfileCached() {
		t.Fatalf("dry-run exit %d stderr=%q cache kept=%v", code, stderr, repoLastProfileCached())
	}
	if recs := vault.AgentRecords(agent.RosterNames()); recs["codex"].Profile != "chatgpt" || recs["claude"].Profile == "qa" {
		t.Fatalf("dry-run wrote records: %+v", recs)
	}
}

// switch skips an agent that cannot serve the profile, clears the repo cache; --dry-run writes neither.
func TestSwitchSkipsUnservableAndClearsRepoCache(t *testing.T) {
	srv, _ := dbxCLIServer(t)
	qaProfile(t, srv.URL, false)
	t.Chdir(t.TempDir())
	for name, p := range map[string]string{"codex": "chatgpt", "gemini": "antigravity"} {
		if err := vault.SetAgentRecord(name, vault.AgentRecord{Profile: p}); err != nil {
			t.Fatal(err)
		}
	}
	providers.RecordLastProfile("codex", "chatgpt")
	if !repoLastProfileCached() {
		t.Fatal("fixture: no last-profile cache written")
	}
	assertSwitchDryRunWritesNothing(t)
	if code, _, stderr := captureOutput(t, func() int { return Switch([]string{"qa"}) }); code != 0 {
		t.Fatalf("exit %d stderr=%q", code, stderr)
	}
	recs := vault.AgentRecords(agent.RosterNames())
	if recs["codex"].Profile != "qa" || recs["gemini"].Profile != "antigravity" {
		t.Errorf("codex=%q gemini=%q, want qa and gemini untouched", recs["codex"].Profile, recs["gemini"].Profile)
	}
	if repoLastProfileCached() {
		t.Error("switch left the repo last-profile cache")
	}
}

func TestAgentRecordCommandsRejectUnreadableRecords(t *testing.T) {
	for _, args := range [][]string{{"openai", "--dry-run"}, {"openai"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			cliScratch(t)
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
			code, out, stderr := captureOutput(t, func() int { return Switch(args) })
			context := "switch profile: "
			if len(args) > 1 {
				context = "switch preview: "
			}
			if code != 1 || out != "" || !strings.Contains(stderr, context+"vault: agent-records is corrupt") {
				t.Fatalf("switch: exit=%d stdout=%q stderr=%q", code, out, stderr)
			}
		})
	}
}

func TestPrintRecordDiffRejectsUnreadableRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	applied := false
	code, out, stderr := captureOutput(t, func() int {
		return printRecordDiff(func(map[string]vault.AgentRecord) { applied = true }, nil)
	})
	if code != 1 || applied || out != "" || !strings.Contains(stderr, "agent record preview: vault: agent-records is corrupt") {
		t.Fatalf("preview: exit=%d applied=%v stdout=%q stderr=%q", code, applied, out, stderr)
	}
}
