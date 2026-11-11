package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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

const dbxListing = `{"endpoints":[
{"name":"databricks-glm-5-2","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-kimi-k3","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-boom-1","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-slow-1","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-embed-1","task":"llm/v1/embeddings","state":{"ready":"READY"}}]}`

// fakeDbxProfile registers a databricks-preset PAT provider `name` pointed at host.
func fakeDbxProfile(t *testing.T, name, host string) {
	t.Helper()
	err := providers.SetProviderFields(name, map[string]any{"preset": "databricks", "host": host,
		"auth": map[string]any{"type": "pat"}})
	if err != nil {
		t.Fatal(err)
	}
}

func ocMenu(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return runOpencodeMenu(args) })
}

func menuIDs() []string {
	var ids []string
	for _, m := range providers.OpencodeMenu() {
		ids = append(ids, m.ID)
	}
	return ids
}

const ocCheckToken = "dapi-check-secret"

type ocCheckFake struct {
	mu    sync.Mutex
	calls map[string]int
	tools map[string]bool // last body carried tools, by path
}

func (f *ocCheckFake) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.calls {
		if strings.Contains(p, sub) {
			n += c
		}
	}
	return n
}

// ocCheckEnv isolates vault+state and serves a listing plus /invocations and /responses handlers.
func ocCheckEnv(t *testing.T) (*ocCheckFake, string) {
	t.Helper()
	dir := vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DATABRICKS_OCFAKE_PAT", ocCheckToken)
	f := &ocCheckFake{calls: map[string]int{}, tools: map[string]bool{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.URL.Path]++
		f.mu.Unlock()
		if r.URL.Path == "/api/2.0/serving-endpoints" {
			_, _ = w.Write([]byte(dbxListing))
			return
		}
		body, _ := io.ReadAll(r.Body)
		hasTools := strings.Contains(string(body), `"tools"`)
		switch {
		case strings.Contains(r.URL.Path, "boom-1"):
			http.Error(w, "internal exploded", 500)
		case strings.Contains(r.URL.Path, "kimi-k3") && hasTools:
			http.Error(w, "tools unsupported", 400)
		case strings.Contains(r.URL.Path, "slow-1"):
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	useTLSTransport(t, srv)
	fakeDbxProfile(t, "ocfake", srv.URL)
	return f, dir
}

type ocCheckRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	HTTP      int    `json:"http"`
	LatencyMS int64  `json:"latency_ms"`
	Tools     string `json:"tools"`
	Error     string `json:"error"`
}

func ocCheckJSON(t *testing.T, args ...string) (int, []ocCheckRow, string, string) {
	t.Helper()
	code, stdout, stderr := ocMenu(t, append([]string{"check", "--profile", "ocfake", "--format", "json"}, args...)...)
	var rows []ocCheckRow
	if code == 0 {
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("%v\n%s", err, stdout)
		}
	}
	return code, rows, stdout, stderr
}

func lastMenuEntry() (providers.OpencodeModel, int) {
	menu := providers.OpencodeMenu()
	return menu[len(menu)-1], len(menu)
}

func TestOpencodeMenuAddRefusesUnserved(t *testing.T) {
	ocCheckEnv(t)
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "bogus-model"); code == 0 || !strings.Contains(stderr, "not served") {
		t.Fatalf("bogus add: code=%d stderr=%q", code, stderr)
	}
	if providers.OpencodeMenuFromVault() {
		t.Fatal("a refused add must not write the vault")
	}
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "databricks-glm-5-2"); code != 0 {
		t.Fatalf("served add: %d %s", code, stderr)
	}
	last, n := lastMenuEntry()
	if last.ID != "glm-5-2" || last.Label != "GLM 5.2" || last.Responses || n != len(providers.DatabricksOpencodeMenu)+1 {
		t.Errorf("menu tail = %+v (len %d)", last, n)
	}
}

func TestOpencodeMenuAddForceBypassesWorkspaceCheck(t *testing.T) {
	f, _ := ocCheckEnv(t)
	if code, _, _ := ocMenu(t, "add", "--profile", "ocfake", "--force", "bogus-model=Bogus"); code != 0 {
		t.Fatal("--force must bypass the workspace check")
	}
	if n := f.count(""); n != 0 { // every path contains "", so this counts all requests, listing included
		t.Errorf("--force sent %d workspace requests, want 0", n)
	}
	if g, _ := lastMenuEntry(); g.ID != "bogus-model" || g.Label != "Bogus" {
		t.Errorf("forced entry = %+v", g)
	}
}

func TestOpencodeMenuAddGPTDefaultsResponses(t *testing.T) {
	ocCheckEnv(t)
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "gpt-5-5"); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	menu := providers.OpencodeMenu()
	if g := menu[len(menu)-1]; g.ID != "gpt-5-5" || !g.Responses {
		t.Errorf("gpt add = %+v", g)
	}
}

func TestOpencodeMenuRemoveAndReset(t *testing.T) {
	ocCheckEnv(t)
	if code, _, stderr := ocMenu(t, "remove", "kimi-k3"); code != 0 {
		t.Fatalf("remove: %d %s", code, stderr)
	}
	for _, id := range menuIDs() {
		if id == "kimi-k3" {
			t.Fatal("kimi-k3 still in menu")
		}
	}
	if !providers.OpencodeMenuFromVault() {
		t.Error("remove must persist the overlay")
	}
	if code, _, stderr := ocMenu(t, "remove", "kimi-k3"); code == 0 || !strings.Contains(stderr, "not in the menu") {
		t.Errorf("second remove: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := ocMenu(t, "reset"); code != 0 {
		t.Fatalf("reset: %d %s", code, stderr)
	}
	if providers.OpencodeMenuFromVault() || len(providers.OpencodeMenu()) != len(providers.DatabricksOpencodeMenu) {
		t.Error("reset must delete the key and restore the seed")
	}
}

func writeImportFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpencodeMenuImportDryRunThenApply(t *testing.T) {
	ocCheckEnv(t)
	plain := writeImportFile(t, "list.txt", "glm-5-2\ngpt-5-5,Five\n")
	code, stdout, stderr := ocMenu(t, "import", plain)
	if code != 0 || !strings.Contains(stdout, "dry run") || !strings.Contains(stdout, "+ glm-5-2") {
		t.Fatalf("dry run: %d out=%q err=%q", code, stdout, stderr)
	}
	if providers.OpencodeMenuFromVault() {
		t.Fatal("dry run wrote the vault")
	}
	if code, _, stderr := ocMenu(t, "import", "--apply", plain); code != 0 {
		t.Fatalf("apply: %d %s", code, stderr)
	}
	menu := providers.OpencodeMenu()
	if len(menu) != 2 || menu[0].ID != "glm-5-2" || menu[1] != (providers.OpencodeModel{ID: "gpt-5-5", Label: "Five", Responses: true}) {
		t.Errorf("menu after apply = %+v", menu)
	}
}

func TestOpencodeMenuImportMissingFileFails(t *testing.T) {
	ocCheckEnv(t)
	if code, _, stderr := ocMenu(t, "import", filepath.Join(t.TempDir(), "missing")); code == 0 || stderr == "" {
		t.Error("missing file must fail")
	}
}

func TestOpencodeMenuListLive(t *testing.T) {
	ocCheckEnv(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"}, {ID: "ghost", Label: "Ghost"}}); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := ocMenu(t, "list", "--live", "--profile", "ocfake", "--format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(rows) != 2 || rows[0]["live"] != "served" || rows[1]["live"] != "missing" || rows[0]["source"] != "vault" {
		t.Errorf("rows = %v", rows)
	}
	if strings.Contains(stdout+stderr, ocCheckToken) {
		t.Error("token leaked")
	}
	assertLiveText(t)
}

func assertLiveText(t *testing.T) {
	t.Helper()
	_, text, _ := ocMenu(t, "list", "--live", "--profile", "ocfake")
	if !strings.Contains(text, "served") || !strings.Contains(text, "missing") {
		t.Errorf("text list:\n%s", text)
	}
}

func TestOpencodeMenuListDefaultSource(t *testing.T) {
	ocCheckEnv(t)
	code, stdout, _ := ocMenu(t, "list", "--format", "json")
	var rows []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &rows) != nil || len(rows) != len(providers.DatabricksOpencodeMenu) || rows[0]["source"] != "default" {
		t.Errorf("code=%d rows=%v", code, rows)
	}
}

func TestOpencodeMenuHelpWithin80Cols(t *testing.T) {
	ocCheckEnv(t)
	for _, verb := range opencodeMenuVerbs {
		_, stdout, stderr := ocMenu(t, verb, "-h")
		out := stdout + stderr
		if !strings.Contains(out, "usage: claude-code opencode catalog "+verb) {
			t.Errorf("%s -h missing canonical usage: %s", verb, out)
		}
		for l := range strings.SplitSeq(out, "\n") {
			if n := len([]rune(l)); n > 80 {
				t.Errorf("%s help line %d cols: %q", verb, n, l)
			}
		}
	}
}

func assertProbeRows(t *testing.T, f *ocCheckFake, rows []ocCheckRow) {
	t.Helper()
	got := map[string]ocCheckRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	assertProbeServed(t, f, got)
	assertProbeFailed(t, f, got)
}

func assertProbeServed(t *testing.T, f *ocCheckFake, got map[string]ocCheckRow) {
	t.Helper()
	if r := got["glm-5-2"]; r.Status != "ok" || r.Tools != "yes" || r.HTTP != 200 {
		t.Errorf("glm = %+v", r)
	}
	if r := got["kimi-k3"]; r.Status != "ok" || r.Tools != "no" || r.HTTP != 200 {
		t.Errorf("kimi = %+v", r)
	}
	if n := f.count("kimi-k3"); n != 2 {
		t.Errorf("kimi requests = %d, want 2 (with tools, then without)", n)
	}
}

func assertProbeFailed(t *testing.T, f *ocCheckFake, got map[string]ocCheckRow) {
	t.Helper()
	if r := got["boom-1"]; r.Status != "transient" || r.HTTP != 500 || !strings.HasPrefix(r.Error, "HTTP 500:") {
		t.Errorf("boom = %+v", r)
	}
	if r := got["slow-1"]; r.Status != "transient" || !strings.Contains(r.Error, "timeout after") {
		t.Errorf("slow = %+v", r)
	}
	if r := got["ghost"]; r.Status != "missing" || r.HTTP != 0 {
		t.Errorf("ghost = %+v", r)
	}
	if n := f.count("ghost"); n != 0 {
		t.Errorf("missing id sent %d requests", n)
	}
}

func assertProbeJSONShape(t *testing.T, stdout string) {
	t.Helper()
	for _, key := range []string{`"id"`, `"status"`, `"http"`, `"latency_ms"`, `"tools"`} {
		if !strings.Contains(stdout, key) {
			t.Errorf("json lacks %s", key)
		}
	}
	if strings.Contains(stdout, `"error": ""`) {
		t.Errorf("error must be omitted when empty:\n%s", stdout)
	}
}

func assertProbeCache(t *testing.T, dir string) {
	t.Helper()
	cache := catalog.CheckCachePath()
	if !strings.HasPrefix(cache, dir) {
		t.Fatalf("cache %s escaped temp dir %s", cache, dir)
	}
	st, err := os.Stat(cache)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cache stat %v %v", st, err)
	}
	raw, _ := os.ReadFile(cache)
	if strings.Contains(string(raw), ocCheckToken) {
		t.Error("token in cache")
	}
	if m, ok := catalog.LoadCheckCache("ocfake"); !ok || len(m) != 5 {
		t.Errorf("cache load = %v %v", m, ok)
	}
	if _, ok := catalog.LoadCheckCache("other"); ok {
		t.Error("cache must be scoped to its provider")
	}
}

func TestOpencodeCheckProbes(t *testing.T) {
	f, dir := ocCheckEnv(t)
	err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "G"}, {ID: "kimi-k3", Label: "K"},
		{ID: "boom-1", Label: "B"}, {ID: "slow-1", Label: "S"}, {ID: "ghost", Label: "Ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	code, rows, stdout, stderr := ocCheckJSON(t, "--timeout", "500ms")
	if code != 0 {
		t.Fatalf("code %d %s", code, stderr)
	}
	assertProbeRows(t, f, rows)
	assertProbeJSONShape(t, stdout)
	assertProbeCache(t, dir)
	if strings.Contains(stdout+stderr, ocCheckToken) {
		t.Error("token in output")
	}
}

func TestOpencodeCheckResponsesRoute(t *testing.T) {
	f, _ := ocCheckEnv(t)
	code, rows, _, stderr := ocCheckJSON(t, "gpt-5-5")
	if code != 0 || len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("code=%d rows=%+v %s", code, rows, stderr)
	}
	if f.count("/responses") == 0 {
		t.Errorf("gpt id must hit the Responses path, calls=%v", f.calls)
	}
	if f.count("gpt-5-5/invocations") != 0 {
		t.Errorf("gpt id must not use /invocations, calls=%v", f.calls)
	}
}

func TestOpencodeCheckExplicitIDs(t *testing.T) {
	f, _ := ocCheckEnv(t)
	code, rows, _, _ := ocCheckJSON(t, "databricks-glm-5-2", "nope")
	if code != 0 || len(rows) != 2 {
		t.Fatalf("code=%d rows=%+v", code, rows)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "databricks-") {
			t.Errorf("id not canonical: %s", r.ID)
		}
	}
	if f.count("nope") != 0 {
		t.Error("unlisted explicit id was probed")
	}
}

func TestOpencodeCheckAllSkipsNonChat(t *testing.T) {
	f, _ := ocCheckEnv(t)
	code, rows, _, _ := ocCheckJSON(t, "--all", "--timeout", "30ms")
	if code != 0 {
		t.Fatalf("--all code %d", code)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "embed") {
			t.Errorf("non-chat endpoint probed: %+v", r)
		}
	}
	if len(rows) != 5 {
		t.Errorf("--all rows = %+v", rows)
	}
	if f.count("embed-1") != 0 {
		t.Error("embeddings endpoint received a request")
	}
}

func TestOpencodeCheckTextOutput(t *testing.T) {
	ocCheckEnv(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "G"}, {ID: "boom-1", Label: "B"}}); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := ocMenu(t, "check", "--profile", "ocfake")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	for _, w := range []string{"ID", "STATUS", "HTTP", "LATENCY", "TOOLS", "HTTP 500"} {
		if !strings.Contains(stdout, w) {
			t.Errorf("text lacks %q:\n%s", w, stdout)
		}
	}
	if strings.Contains(stdout+stderr, ocCheckToken) {
		t.Error("token leaked")
	}
}

func TestOpencodeCheckFlagErrors(t *testing.T) {
	ocCheckEnv(t)
	if code, _, _ := ocMenu(t, "check", "--bogus"); code != 2 {
		t.Errorf("bad flag code = %d, want 2", code)
	}
	if code, _, _ := ocMenu(t, "check", "--format", "xml"); code != 2 {
		t.Errorf("bad format code = %d, want 2", code)
	}
	if code, _, stderr := ocMenu(t, "check", "--profile", "no-such-provider"); code != 1 || stderr == "" {
		t.Errorf("unknown provider: code=%d stderr=%q", code, stderr)
	}
	if code, _, _, stderr := ocCheckJSON(t, "--all", "glm-5-2"); code != 1 || stderr == "" {
		t.Errorf("--all with ids: code=%d stderr=%q", code, stderr)
	}
}

// liveCheckColumns returns the list --live JSON `check` column by model id.
func liveCheckColumns(t *testing.T) map[string]string {
	t.Helper()
	code, stdout, stderr := ocMenu(t, "list", "--live", "--profile", "ocfake", "--format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		c, _ := r["check"].(string)
		out[r["id"].(string)] = c
	}
	return out
}

func TestOpencodeListLiveCheckColumn(t *testing.T) {
	ocCheckEnv(t)
	err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "G"}, {ID: "kimi-k3", Label: "K"},
		{ID: "boom-1", Label: "B"}, {ID: "ghost", Label: "Ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	// negative control: no cache yet
	for id, c := range liveCheckColumns(t) {
		if c != "-" {
			t.Errorf("no cache: %s check = %q, want -", id, c)
		}
	}
	_, text, _ := ocMenu(t, "list", "--live", "--profile", "ocfake")
	if !strings.Contains(text, "CHECK") {
		t.Errorf("no CHECK header:\n%s", text)
	}
	if code, _, _, stderr := ocCheckJSON(t, "glm-5-2", "kimi-k3", "boom-1", "ghost"); code != 0 {
		t.Fatalf("check: %d %s", code, stderr)
	}
	want := map[string]string{"glm-5-2": "ok tools", "kimi-k3": "ok no-tools", "boom-1": "transient 500", "ghost": "missing"}
	got := liveCheckColumns(t)
	if len(got) != len(want) {
		t.Errorf("rows = %v, want %d rows", got, len(want))
	}
	for id, c := range want {
		if v, ok := got[id]; !ok || v != c {
			t.Errorf("%s check = %q (present %v), want %q", id, v, ok, c)
		}
	}
	// a cache for another provider must not show
	if err := catalog.SaveCheckCache(catalog.CheckCache{Provider: "other",
		Results: []serving.CheckResult{{ID: "glm-5-2", Status: "ok", Tools: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	if c := liveCheckColumns(t)["glm-5-2"]; c != "-" {
		t.Errorf("foreign cache shown: %q", c)
	}
}

const syncListing = `{"endpoints":[
{"name":"databricks-sync-zeta","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-alpha","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-alpha","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-6-astra","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-not-ready","task":"llm/v1/chat","state":{"ready":"NOT_READY"}},
{"name":"databricks-sync-embed","task":"llm/v1/embeddings","state":{"ready":"READY"}},
{"name":"unprefixed-chat","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-marked[1m]","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-spaced ","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-","task":"llm/v1/chat","state":{"ready":"READY"}}]}`

type syncTransport func(*http.Request) (*http.Response, error)

func (f syncTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type syncFake struct {
	p        *providers.Provider
	me, list atomic.Int32
	meCode   atomic.Int32
	listCode atomic.Int32
	listing  atomic.Value
}

// serve answers identity, discovery and (via probe) verification requests with the fake's current codes.
func (f *syncFake) serve(t *testing.T, probe []http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-sync-credential" ||
			(strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet) {
			t.Error("expected vault credential and GET for discovery/identity")
		}
		switch r.URL.Path {
		case "/api/2.0/preview/scim/v2/Me":
			f.serveMe(t, w, r)
		case "/api/2.0/serving-endpoints":
			f.list.Add(1)
			w.WriteHeader(int(f.listCode.Load()))
			_, _ = w.Write([]byte(f.listing.Load().(string)))
		default:
			if len(probe) > 0 {
				probe[0](w, r)
				return
			}
			t.Errorf("sync without --verify made unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected path", 404)
		}
	}
}

func (f *syncFake) serveMe(t *testing.T, w http.ResponseWriter, r *http.Request) {
	f.me.Add(1)
	if r.URL.Query().Get("attributes") != "id,active" {
		t.Errorf("Me query = %s", r.URL.RawQuery)
	}
	w.WriteHeader(int(f.meCode.Load()))
	if f.meCode.Load() != 200 {
		_, _ = w.Write([]byte("private-upstream-body synthetic-sync-credential"))
		return
	}
	_, _ = w.Write([]byte(`{"id":"sync-subject","active":true}`))
}

func syncEnv(t *testing.T, probe ...http.HandlerFunc) *syncFake {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DATABRICKS_SYNCTEST_PAT", "")
	f := &syncFake{}
	f.meCode.Store(200)
	f.listCode.Store(200)
	f.listing.Store(syncListing)
	srv := httptest.NewTLSServer(f.serve(t, probe))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = syncTransport(func(r *http.Request) (*http.Response, error) {
		if "https://"+r.URL.Host != srv.URL {
			return nil, errors.New("test blocked nonlocal request")
		}
		return srv.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	fakeDbxProfile(t, "synctest", srv.URL)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	f.p, err = reg.Get("synctest")
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, providers.AuthSecretName(f.p), "synthetic-sync-credential")
	return f
}

func syncVaultBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func syncRun(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := ocMenu(t, append([]string{"sync", "--profile", "synctest"}, args...)...)
	if code != 0 {
		t.Fatalf("sync %v: code=%d stderr=%s", args, code, stderr)
	}
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") {
		t.Fatal("sync exposed credentials")
	}
	return stdout
}

func syncExistingMenu(t *testing.T) []providers.OpencodeModel {
	t.Helper()
	existing := []providers.OpencodeModel{
		{ID: "retained-absent", Label: "Keep first", Responses: true},
		{ID: "gpt-5-5", Label: "Explicit chat override", Responses: false},
		{ID: "glm-5-2", Label: "Explicit Responses override", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	return existing
}

func TestOpencodeMenuSyncPreviewApplyFiltersAndIdempotence(t *testing.T) {
	f := syncEnv(t)
	existing := syncExistingMenu(t)
	before := syncVaultBytes(t)
	for _, args := range [][]string{nil, {"--dry-run"}} {
		out := syncRun(t, args...)
		if !strings.Contains(out, "+ sync-alpha") || !strings.Contains(out, "dry run") || !bytes.Equal(before, syncVaultBytes(t)) {
			t.Fatalf("preview wrote vault or omitted additions: %s", out)
		}
	}
	syncRun(t, "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 6 || !reflect.DeepEqual(got[:3], existing) {
		t.Fatalf("additive merge changed existing entries or included rejected endpoints: %+v", got)
	}
	wantNew := []providers.OpencodeModel{
		{ID: "gpt-6-astra", Label: "GPT 6 Astra", Responses: true},
		{ID: "sync-alpha", Label: "Sync Alpha"},
		{ID: "sync-zeta", Label: "Sync Zeta"},
	}
	if !reflect.DeepEqual(got[3:], wantNew) {
		t.Fatalf("new models must be canonical, unique, sorted with inferred Responses: %+v", got[3:])
	}
	if f.me.Load() != 3 || f.list.Load() != 3 {
		t.Fatalf("previews must not cache: Me=%d listing=%d", f.me.Load(), f.list.Load())
	}
}

func TestOpencodeMenuSyncUnchangedApplyHitsCache(t *testing.T) {
	f := syncEnv(t)
	syncExistingMenu(t)
	syncRun(t, "--apply")
	sealed := syncVaultBytes(t)
	out := syncRun(t, "--apply")
	if f.me.Load() != 2 || f.list.Load() != 1 || !strings.Contains(out, "encrypted cache hit") || !bytes.Equal(sealed, syncVaultBytes(t)) {
		t.Fatalf("unchanged apply must authenticate, not list or write: Me=%d listing=%d out=%s", f.me.Load(), f.list.Load(), out)
	}
}

// syncNoAdditionsApply previews then applies a zero-addition listing; returns the pre-apply and sealed vault bytes.
func syncNoAdditionsApply(t *testing.T) (before, sealed []byte) {
	t.Helper()
	before = syncVaultBytes(t)
	syncRun(t, "--dry-run")
	if !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatal("no-additions preview wrote cache")
	}
	out := syncRun(t, "--apply")
	if !strings.Contains(out, "no menu change") || providers.OpencodeMenuFromVault() {
		t.Fatalf("empty/no-change discovery must not write menu: %s", out)
	}
	sealed = syncVaultBytes(t)
	if bytes.Equal(before, sealed) {
		t.Fatal("apply failed to persist discovery with zero additions")
	}
	return before, sealed
}

// assertNoAdditionsCaching checks the cached hit, the --refresh rewrite and the refreshed snapshot's reuse.
func assertNoAdditionsCaching(t *testing.T, f *syncFake, sealed []byte) {
	t.Helper()
	syncRun(t, "--apply")
	if f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(sealed, syncVaultBytes(t)) {
		t.Fatalf("zero-addition cache hit rewrote or listed: Me=%d listing=%d", f.me.Load(), f.list.Load())
	}
	assertNoAdditionsRefresh(t, f, sealed)
}

func assertNoAdditionsRefresh(t *testing.T, f *syncFake, sealed []byte) {
	t.Helper()
	syncRun(t, "--refresh", "--apply")
	refreshed := syncVaultBytes(t)
	if f.me.Load() != 4 || f.list.Load() != 3 || bytes.Equal(sealed, refreshed) || providers.OpencodeMenuFromVault() {
		t.Fatal("zero-addition refresh must update discovery, not the menu")
	}
	syncRun(t, "--apply")
	if f.me.Load() != 5 || f.list.Load() != 3 || !bytes.Equal(refreshed, syncVaultBytes(t)) {
		t.Fatal("refreshed snapshot was not reused without writes")
	}
}

func TestOpencodeMenuSyncNoAdditionsStillSealsDiscovery(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", `{"endpoints":[]}`},
		{"already-in-menu", `{"endpoints":[{"name":"databricks-kimi-k3","task":"llm/v1/chat","state":{"ready":"READY"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := syncEnv(t)
			f.listing.Store(tc.body)
			_, sealed := syncNoAdditionsApply(t)
			assertNoAdditionsCaching(t, f, sealed)
		})
	}
}

// assertRejectedBeforeHTTP runs ocMenu args and requires a failure with no HTTP request and no vault write.
func assertRejectedBeforeHTTP(t *testing.T, f *syncFake, what string, args ...string) {
	t.Helper()
	before := syncVaultBytes(t)
	if code, _, _ := ocMenu(t, args...); code == 0 || f.me.Load() != 0 || f.list.Load() != 0 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("%s: %v, code=%d", what, args, code)
	}
}

func TestOpencodeMenuSyncControlsBeforeHTTP(t *testing.T) {
	f := syncEnv(t)
	vaulttest.SetRecord(t, "opencode", "profile", "synctest")
	for _, args := range [][]string{
		{"sync"},
		{"sync", "--profile", "synctest", "--apply", "--dry-run"},
		{"sync", "--profile", "synctest", "unexpected"},
	} {
		assertRejectedBeforeHTTP(t, f, "invalid controls reached HTTP or wrote state", args...)
	}
	if err := providers.SetProviderFields("synctest", map[string]any{"options": map[string]any{"wire": "ai-gateway"}}); err != nil {
		t.Fatal(err)
	}
	assertRejectedBeforeHTTP(t, f, "gateway profile was not rejected before HTTP/writes", "sync", "--profile", "synctest", "--apply")
}

func assertNoSyncLeak(t *testing.T, stdout, stderr, what string) {
	t.Helper()
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") || strings.Contains(stdout+stderr, "private-upstream-body") {
		t.Fatal(what + " leaked sensitive text")
	}
}

func TestOpencodeMenuSyncFailuresNeverUseStaleOrWrite(t *testing.T) {
	f := syncEnv(t)
	syncRun(t, "--apply")
	before := syncVaultBytes(t)
	f.meCode.Store(401)
	code, stdout, stderr := ocMenu(t, "sync", "--profile", "synctest", "--apply")
	if code == 0 || f.me.Load() != 2 || f.list.Load() != 1 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("revoked auth used cache or wrote state: code=%d Me=%d listing=%d", code, f.me.Load(), f.list.Load())
	}
	assertNoSyncLeak(t, stdout, stderr, "identity error")
	f.meCode.Store(200)
	f.listCode.Store(503)
	f.listing.Store("private-upstream-body synthetic-sync-credential")
	code, stdout, stderr = ocMenu(t, "sync", "--profile", "synctest", "--refresh", "--apply")
	if code == 0 || f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("failed refresh used stale data or wrote state: code=%d Me=%d listing=%d", code, f.me.Load(), f.list.Load())
	}
	assertNoSyncLeak(t, stdout, stderr, "listing error")
}

// seedHistoricalFailures saves failed probe results for an existing and a new model into the sealed snapshot.
func seedHistoricalFailures(t *testing.T, f *syncFake) {
	t.Helper()
	s, err := serving.Discover(context.Background(), f.p, false)
	if err != nil {
		t.Fatal(err)
	}
	s.CheckedAt = time.Now().UTC()
	s.Checks = []serving.CheckResult{
		{ID: "gpt-5-5", Status: "fail", HTTP: 400, Error: "private-upstream-body synthetic-sync-credential"},
		{ID: "sync-alpha", Status: "fail", HTTP: 403, Error: "private-upstream-body"},
	}
	if err := serving.Save(f.p, s); err != nil {
		t.Fatal(err)
	}
}

func TestOpencodeMenuSyncHistoricalFailuresOnlyBlockNewEntries(t *testing.T) {
	f := syncEnv(t)
	existing := []providers.OpencodeModel{{ID: "gpt-5-5", Label: "Keep failed existing", Responses: false}}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	seedHistoricalFailures(t, f)
	out := syncRun(t, "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 3 || got[0] != existing[0] || slices.Contains(menuIDs(), "sync-alpha") ||
		!strings.Contains(out, "historical probes") || !strings.Contains(out, "1 failed additions skipped") {
		t.Fatalf("historical checks must block only failed NEW entries: menu=%+v out=%s", got, out)
	}
	if f.me.Load() != 2 || f.list.Load() != 1 || strings.Contains(out, "private-upstream-body") {
		t.Fatal("historical cache was not reused safely")
	}
	assertNoLegacyCheckCache(t, "sync touched legacy plaintext check cache")
}

func assertNoLegacyCheckCache(t *testing.T, msg string) {
	t.Helper()
	if _, err := os.Stat(catalog.CheckCachePath()); !os.IsNotExist(err) {
		t.Fatalf("%s: %v", msg, err)
	}
}

// verifyProbeHandler serves --verify probes: chat completions are rejected (403), Responses succeed.
func verifyProbeHandler(t *testing.T, chat, responses *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected JSON probe POST")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("probe JSON: %v", err)
		}
		switch r.URL.Path {
		case "/serving-endpoints/chat/completions":
			chat.Add(1)
			if body["model"] != "databricks-gpt-5-5" && body["model"] != "databricks-sync-rejected" {
				t.Errorf("wrong chat model: %v", body["model"])
			}
			http.Error(w, "private-probe-body synthetic-sync-credential", http.StatusForbidden)
		case "/serving-endpoints/responses":
			responses.Add(1)
			if body["model"] != "databricks-gpt-6-astra" {
				t.Errorf("wrong Responses model: %v", body["model"])
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected verification route: %s", r.URL.Path)
			http.Error(w, "unexpected path", 404)
		}
	}
}

func assertSealedChecks(t *testing.T, f *syncFake) {
	t.Helper()
	snapshot, err := serving.Discover(context.Background(), f.p, false)
	if err != nil || !snapshot.CacheHit || snapshot.CheckedAt.IsZero() || len(snapshot.Checks) != 3 {
		t.Fatalf("sealed verification snapshot missing: %+v err=%v", snapshot, err)
	}
	for _, check := range snapshot.Checks {
		wantStatus, wantHTTP := "fail", http.StatusForbidden
		if check.ID == "gpt-6-astra" {
			wantStatus, wantHTTP = "ok", http.StatusOK
		}
		if check.Status != wantStatus || check.HTTP != wantHTTP || check.Error != "" {
			t.Errorf("unsanitized or incorrect saved check: %+v", check)
		}
	}
}

// sealedDiscoveryRaw reads the one decrypted model-discovery record, so a persisted raw body cannot hide.
func sealedDiscoveryRaw(t *testing.T) string {
	t.Helper()
	var discoveryKeys []string
	if err := vault.View(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.Secrets) {
			if strings.HasPrefix(name, "model-discovery/") {
				discoveryKeys = append(discoveryKeys, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(discoveryKeys) != 1 {
		t.Fatalf("discovery records = %d, want one", len(discoveryKeys))
	}
	raw, found, err := vault.ReadSecret(discoveryKeys[0])
	if err != nil || !found {
		t.Fatalf("read sealed snapshot: found=%v err=%v", found, err)
	}
	return raw
}

// verifyFixture serves a three-endpoint listing and seeds a menu with one flagged and one discovery-absent entry.
func verifyFixture(t *testing.T) (f *syncFake, chat, responses *atomic.Int32, existing []providers.OpencodeModel) {
	t.Helper()
	chat, responses = new(atomic.Int32), new(atomic.Int32)
	f = syncEnv(t, verifyProbeHandler(t, chat, responses))
	f.listing.Store(`{"endpoints":[
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-6-astra","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-rejected","task":"llm/v1/chat","state":{"ready":"READY"}}]}`)
	existing = []providers.OpencodeModel{
		{ID: "gpt-5-5", Label: "Keep explicit chat flag", Responses: false},
		{ID: "absent-from-discovery", Label: "Keep absent", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	return f, chat, responses, existing
}

func assertVerifiedMenu(t *testing.T, existing []providers.OpencodeModel, out string) {
	t.Helper()
	got := providers.OpencodeMenu()
	if len(got) != 3 || !reflect.DeepEqual(got[:2], existing) || got[2].ID != "gpt-6-astra" || !got[2].Responses ||
		!strings.Contains(out, "1 failed additions skipped") || !strings.Contains(out, "fresh probes") {
		t.Fatalf("verified merge failed: menu=%+v output=%s", got, out)
	}
}

func assertNoProbeLeak(t *testing.T, printed string) {
	t.Helper()
	raw := sealedDiscoveryRaw(t)
	for _, forbidden := range []string{"private-probe-body", "synthetic-sync-credential"} {
		if strings.Contains(raw+printed, forbidden) {
			t.Error("verification persisted or printed private probe diagnostics")
		}
	}
}

func assertVerifiedHitIsQuiet(t *testing.T, f *syncFake, chat, responses *atomic.Int32) {
	t.Helper()
	sealed := syncVaultBytes(t)
	syncRun(t, "--apply")
	if !bytes.Equal(sealed, syncVaultBytes(t)) || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatal("historical verified hit must not reprobe, relist, or rewrite")
	}
}

func TestOpencodeMenuSyncVerifySealsSafeChecksAndPreservesExisting(t *testing.T) {
	f, chat, responses, existing := verifyFixture(t)
	before := syncVaultBytes(t)
	preview := syncRun(t, "--verify", "--dry-run")
	if !bytes.Equal(before, syncVaultBytes(t)) || chat.Load() != 4 || responses.Load() != 1 {
		t.Fatal("verification preview must probe without persisting results or menu")
	}
	out := syncRun(t, "--verify", "--apply")
	assertVerifiedMenu(t, existing, out)
	if f.me.Load() != 2 || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatalf("wrong verification requests: Me=%d listing=%d chat=%d responses=%d", f.me.Load(), f.list.Load(), chat.Load(), responses.Load())
	}
	assertSealedChecks(t, f)
	assertNoProbeLeak(t, preview+out)
	assertNoLegacyCheckCache(t, "verification touched legacy plaintext cache")
	assertVerifiedHitIsQuiet(t, f, chat, responses)
}
