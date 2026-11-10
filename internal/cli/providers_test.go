package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/describe"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
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
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdModels(tc.args) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return listLive(reg, p, helptext.Text) })
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
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return listLive(reg, p, helptext.JSON) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return listLive(reg, p, helptext.Text) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return listLive(&providers.Registry{}, dbxCLIProvider(srv.URL), helptext.Text)
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 4 || strings.Contains(stdout, "AI GATEWAY") {
		t.Fatalf("want header + 3 rows and no AI Gateway section for a serving row:\n%s", stdout)
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
	assertNoDbxTokenLeak(t, stdout, stderr)
}

func assertDbxJSONRows(t *testing.T, raw []map[string]any) {
	t.Helper()
	if len(raw) != 3 || slices.ContainsFunc(raw, func(r map[string]any) bool { return r["gateway"] != nil }) {
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
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
	_, text, _ := hosttest.CaptureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), helptext.Text) })
	if !strings.HasPrefix(text, "ID") || strings.Contains(text, "TASK") {
		t.Errorf("text = %q, want the ID/Model table", text)
	}
	_, js, _ := hosttest.CaptureOutput(t, func() int { return listLive(&providers.Registry{}, fakeProvider(srv.URL), helptext.JSON) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa"}) })
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
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa"}) })
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa", "--dry-run"}) }); code != 0 {
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
	if code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa"}) }); code != 0 || !strings.Contains(out, "pinned") {
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
	_, out, _ := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa", "--dry-run"}) })
	if !strings.Contains(out, "codex.profile: chatgpt → qa") || !strings.Contains(out, "copilot: off (kept)") {
		t.Fatalf("dry-run = %q", out)
	}
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa"}) }); code != 0 {
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa", "--dry-run"}) }); code != 0 || !repoLastProfileCached() {
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
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch([]string{"qa"}) }); code != 0 {
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
			code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdSwitch(args) })
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return printRecordDiff(func(map[string]vault.AgentRecord) { applied = true }, nil)
	})
	if code != 1 || applied || out != "" || !strings.Contains(stderr, "agent record preview: vault: agent-records is corrupt") {
		t.Fatalf("preview: exit=%d applied=%v stdout=%q stderr=%q", code, applied, out, stderr)
	}
}

// providerListCmdJSON runs `configure providers list <name> --format json`, requires exit 0, and returns stdout and stderr.
func providerListCmdJSON(t *testing.T, name string) (stdout, stderr string) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return providerList(name, "json", func() []describe.Verb { return nil }) })
	if code != 0 {
		t.Fatalf("provider list %q JSON exit %d stderr %q", name, code, stderr)
	}
	return stdout, stderr
}

func TestProviderListJSONCarriesHostNoSecret(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const secret = "dapi-list-secret-value-123456"
	err := providers.SetProviderFields("dbx-host", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat", "accountId": "1234abcd-0000", "workspaceId": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Store("DATABRICKS_DBX_HOST_PAT", secret); err != nil {
		t.Fatal(err)
	}
	out, _ := providerListCmdJSON(t, "dbx-host")
	var e struct{ Host, AccountID, WorkspaceID string }
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	e.Host, _ = raw["host"].(string)
	e.AccountID, _ = raw["accountId"].(string)
	e.WorkspaceID, _ = raw["workspaceId"].(string)
	if e.Host != "https://example.cloud.databricks.com" || e.AccountID != "1234abcd-0000" || e.WorkspaceID != "42" {
		t.Errorf("host/ids = %+v\n%s", e, out)
	}
	if strings.Contains(out, secret) {
		t.Error("list output carries the sealed secret")
	}
	all, _ := providerListCmdJSON(t, "")
	if !strings.Contains(all, `"host"`) || strings.Contains(all, secret) {
		t.Error("full list: secret leaked or host missing")
	}
}

func TestProviderListTextWriteError(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if err := providers.SetProviderFields("write-error", map[string]any{
		"protocol": "openai", "base_url": "https://example.invalid",
		"auth": map[string]any{"type": "none"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "write-error"} {
		t.Run("name="+name, func(t *testing.T) {
			run := func() int { return providerList(name, "text", nil) }
			code, stdout, stderr := hosttest.CaptureOutput(t, run)
			if code != 0 || !strings.Contains(stdout, "write-error") || stderr != "" {
				t.Fatalf("successful write: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			code, stderr = captureClosedStdout(t, run)
			if code != 1 || !strings.Contains(stderr, "configure provider:") || !strings.Contains(stderr, "file already closed") {
				t.Fatalf("failed text write: code=%d stderr=%q, want exit 1 and write diagnostic", code, stderr)
			}
		})
	}
}

func summaryRegistry() *providers.Registry {
	dbx := func(name string, auth providers.Auth) *providers.Provider {
		return &providers.Provider{Name: name, Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic, Preset: providers.PresetDatabricks,
			BaseURL: "https://dbc-1.cloud.databricks.com/serving-endpoints/anthropic",
			Routes:  map[string]string{providers.ProtocolResponses: "https://dbc-1.cloud.databricks.com/serving-endpoints"},
			Auth:    auth}
	}
	return &providers.Registry{Default: "dev", Providers: map[string]*providers.Provider{
		"prod":    dbx("prod", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"dev":     dbx("dev", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"sp-user": dbx("sp-user", providers.Auth{Type: providers.AuthM2M, Env: "SUMMARY_SP_SECRET"}),
		"openai": {Name: "openai", Provider: "openai", Protocol: providers.ProtocolOpenAI, BaseURL: "https://api.openai.com/v1",
			Auth: providers.Auth{Type: providers.AuthKey, Env: "SUMMARY_MISSING_KEY"}},
	}}
}

// tableRow returns the first line of out whose first field is label.
func tableRow(out, label string) string {
	for l := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == label {
			return l
		}
	}
	return ""
}

// The summary lists profiles whose credential resolves, folds m2m rows into MODE, and adds an agent section.
func TestProviderTableSummary(t *testing.T) {
	vaulttest.UseFreshKey(t)
	t.Setenv("SUMMARY_MISSING_KEY", "")
	t.Setenv("SUMMARY_SP_SECRET", "x")
	reg := summaryRegistry()
	_, out, _ := hosttest.CaptureOutput(t, func() int { printProviderTable(reg); return 0 })
	if !strings.Contains(out, "PROVIDERS") || !strings.Contains(out, "AUTH") {
		t.Fatalf("no header:\n%s", out)
	}
	if strings.Contains(out, "sp-user") || strings.Contains(out, "openai ") {
		t.Fatalf("m2m row or unconfigured provider listed as a profile:\n%s", out)
	}
	if r := tableRow(out, "databricks"); !strings.Contains(r, "*dev, prod") || !strings.Contains(r, "oauth, m2m") {
		t.Fatalf("databricks row = %q, want profiles *dev, prod and auths oauth, m2m", r)
	}
	if r := tableRow(out, "codex"); !strings.Contains(r, "*dev, prod") {
		t.Fatalf("codex row = %q, want the Databricks profiles it can run on", r)
	}
}

const credSourceValue = "obviously-fake-source-value"

func credSourceName(source string) string { return "fake-" + strings.ReplaceAll(source, ".", "-") }

// credSourceFixture seeds one provider per credential source and returns the sources plus the text provider table.
func credSourceFixture(t *testing.T) (map[string]providers.Auth, string) {
	t.Helper()
	vaulttest.UseFreshKey(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv("SOURCE_ENV_KEY", credSourceValue)
	t.Setenv("SOURCE_VAULT_KEY", "")
	t.Setenv("SOURCE_MISSING_KEY", "")
	if err := vault.Store("SOURCE_VAULT_KEY", credSourceValue); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fake-token")
	if err := os.WriteFile(file, []byte(credSourceValue), 0o600); err != nil {
		t.Fatal(err)
	}
	auths := map[string]providers.Auth{
		"vault":     {Type: providers.AuthKey, Env: "SOURCE_VAULT_KEY"},
		"env":       {Type: providers.AuthKey, Env: "SOURCE_ENV_KEY"},
		"auth.file": {Type: providers.AuthKey, File: file},
		"command":   {Type: providers.AuthKey, Command: "printf obviously-fake-command"},
		"cli":       {Type: providers.AuthCLI, Login: "fake-login"},
		"missing":   {Type: providers.AuthKey, Env: "SOURCE_MISSING_KEY"},
		"none":      {Type: providers.AuthNone},
	}
	rows := map[string]*providers.Provider{}
	for source, auth := range auths {
		name := credSourceName(source)
		rows[name] = &providers.Provider{
			Name: name, Protocol: providers.ProtocolOpenAI, BaseURL: "https://example.invalid", Auth: auth,
		}
	}
	raw, err := json.Marshal(map[string]any{"default": "fake-none",
		"providers": map[string]any{"openai": rows}})
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", string(raw))
	code, text, _ := hosttest.CaptureOutput(t, func() int { return providerList("", "text", nil) })
	if code != 0 {
		t.Fatalf("provider table exit %d", code)
	}
	return auths, text
}

func assertCredentialSource(t *testing.T, source, text string) {
	t.Helper()
	name := credSourceName(source)
	stdout, stderr := providerListCmdJSON(t, name)
	if strings.Contains(stdout+stderr, credSourceValue) {
		t.Fatal("provider JSON leaked a raw value")
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &entry); err != nil {
		t.Fatal(err)
	}
	var got, note string
	_ = json.Unmarshal(entry["source"], &got)
	_ = json.Unmarshal(entry["note"], &note)
	if got != source || entry["note"] == nil {
		t.Errorf("JSON source=%q, want %q; note present=%v", got, source, entry["note"] != nil)
	}
	if source == "missing" && note != "set SOURCE_MISSING_KEY" {
		t.Errorf("missing credential note=%q", note)
	}
	assertCredentialDetail(t, name, source, note, text)
}

func assertCredentialDetail(t *testing.T, name, source, note, text string) {
	t.Helper()
	code, detail, _ := hosttest.CaptureOutput(t, func() int { return providerList(name, "text", nil) })
	if code != 0 || !strings.Contains(detail, "cred      "+source) ||
		(note != "" && !strings.Contains(detail, note)) {
		t.Error("provider text detail omitted source or note")
	}
	if strings.Contains(text+detail, credSourceValue) {
		t.Fatal("provider text leaked a raw value")
	}
}

func TestProviderCredentialSources(t *testing.T) {
	auths, text := credSourceFixture(t)
	for source := range auths {
		t.Run(source, func(t *testing.T) { assertCredentialSource(t, source, text) })
	}
	stdout, _ := providerListCmdJSON(t, "")
	var list struct {
		List []describe.ProviderListEntry `json:"list"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatal("provider list JSON failed")
	}
	for _, entry := range list.List {
		if entry.Source == "" {
			t.Errorf("provider %s has no source", entry.Name)
		}
	}
}

// A qualified <provider>·<profile> name resolves like reg.Get and lists that profile, not panics.
func TestProviderListJSONQualifiedName(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if err := providers.SetProviderFields("grp-dev", map[string]any{
		"provider": "databricks", "protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := providerListCmdJSON(t, "databricks·grp-dev")
	var raw struct{ Name string }
	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw.Name != "grp-dev" {
		t.Fatalf("got name %q err %v from %s, want grp-dev", raw.Name, err, out)
	}
}

func TestPrintDiscoverResultsTextWriteError(t *testing.T) {
	results := []copilot.DiscoverResult{{Provider: "openai", Profile: "fake", Status: "preview", Discovered: 1}}
	for _, status := range []int{0, 1} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return printDiscoverResults(results, false, helptext.Text, status)
		})
		if code != status || !strings.Contains(stdout, "openai/fake: preview") || stderr != "" {
			t.Fatalf("successful write: code=%d, want %d; stdout=%q stderr=%q", code, status, stdout, stderr)
		}
	}
	code, stderr := captureClosedStdout(t, func() int {
		return printDiscoverResults(results, false, helptext.Text, 0)
	})
	if code != 1 || !strings.Contains(stderr, "models discover: cannot write text output") {
		t.Fatalf("failed text write: code=%d stderr=%q, want exit 1 and write diagnostic", code, stderr)
	}
}

func captureClosedStdout(t *testing.T, run func() int) (int, string) {
	t.Helper()
	closed, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		stdout := os.Stdout
		os.Stdout = closed
		defer func() { os.Stdout = stdout }()
		return run()
	})
	return code, stderr
}

// useTLSTransport routes http.DefaultClient through srv's TLS client and closes srv at cleanup.
func useTLSTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
}

// resolve --proxy prints an armed launch's session token when the env carries one, else the router secret.
func TestResolveProxyPrintsArmSessionToken(t *testing.T) {
	cliScratch(t)
	secret, err := gateway.ClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(claude.ProxyKeyEnv, "")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdResolve([]string{"--proxy"}) })
	if code != 0 || strings.TrimSpace(out) != secret {
		t.Errorf("unarmed: exit %d, printed the router secret = %v", code, strings.TrimSpace(out) == secret)
	}
	const tok = "cct_ARM_TEST"
	t.Setenv(claude.ProxyKeyEnv, tok)
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdResolve([]string{"--proxy"}) })
	if got := strings.TrimSpace(out); code != 0 || got != tok {
		t.Errorf("armed: exit %d, printed the token = %v, the router secret = %v (len %d)", code, got == tok, got == secret, len(got))
	}
}
