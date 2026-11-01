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

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	return captureOutput(t, func() int { return AgentVerb("opencode", append([]string{"catalog"}, args...)) })
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
		if r.URL.Path == "/api/2.0/serving-endpoints" {
			_, _ = w.Write([]byte(dbxListing))
			return
		}
		body, _ := io.ReadAll(r.Body)
		hasTools := strings.Contains(string(body), `"tools"`)
		f.mu.Lock()
		f.calls[r.URL.Path]++
		f.mu.Unlock()
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

func TestOpencodeMenuAddRefusesUnservedAndForce(t *testing.T) {
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
	menu := providers.OpencodeMenu()
	last := menu[len(menu)-1]
	if last.ID != "glm-5-2" || last.Label != "GLM 5.2" || last.Responses || len(menu) != len(providers.DatabricksOpencodeMenu)+1 {
		t.Errorf("menu tail = %+v (len %d)", last, len(menu))
	}
	if code, _, _ := ocMenu(t, "add", "--profile", "ocfake", "--force", "bogus-model=Bogus"); code != 0 {
		t.Fatal("--force must bypass the workspace check")
	}
	menu = providers.OpencodeMenu()
	if g := menu[len(menu)-1]; g.ID != "bogus-model" || g.Label != "Bogus" {
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

func TestOpencodeMenuImportDryRunThenApply(t *testing.T) {
	ocCheckEnv(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(plain, []byte("glm-5-2\ngpt-5-5,Five\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	cfg := filepath.Join(dir, "opencode.json")
	doc := `{"provider":{"databricks-qa":{"models":{"databricks-kimi-k3":{"name":"Kimi K3 · qa"}}}}}`
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := ocMenu(t, "import", "--apply", cfg); code != 0 {
		t.Fatalf("json apply: %d %s", code, stderr)
	}
	if got := menuIDs(); len(got) != 1 || got[0] != "kimi-k3" {
		t.Errorf("import must replace the whole menu, got %v", got)
	}
	if code, _, stderr := ocMenu(t, "import", filepath.Join(dir, "missing")); code == 0 || stderr == "" {
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
	got := map[string]ocCheckRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := got["glm-5-2"]; r.Status != "ok" || r.Tools != "yes" || r.HTTP != 200 {
		t.Errorf("glm = %+v", r)
	}
	if r := got["kimi-k3"]; r.Status != "ok" || r.Tools != "no" || r.HTTP != 200 {
		t.Errorf("kimi = %+v", r)
	}
	if n := f.count("kimi-k3"); n != 2 {
		t.Errorf("kimi requests = %d, want 2 (with tools, then without)", n)
	}
	if r := got["boom-1"]; r.Status != "fail" || r.HTTP != 500 || !strings.HasPrefix(r.Error, "HTTP 500:") {
		t.Errorf("boom = %+v", r)
	}
	if r := got["slow-1"]; r.Status != "fail" || !strings.Contains(r.Error, "timeout after") {
		t.Errorf("slow = %+v", r)
	}
	if r := got["ghost"]; r.Status != "missing" || r.HTTP != 0 {
		t.Errorf("ghost = %+v", r)
	}
	if n := f.count("ghost"); n != 0 {
		t.Errorf("missing id sent %d requests", n)
	}
	for _, key := range []string{`"id"`, `"status"`, `"http"`, `"latency_ms"`, `"tools"`} {
		if !strings.Contains(stdout, key) {
			t.Errorf("json lacks %s", key)
		}
	}
	if strings.Contains(stdout, `"error": ""`) {
		t.Errorf("error must be omitted when empty:\n%s", stdout)
	}
	cache := opencode.CheckCachePath()
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
	if strings.Contains(stdout+stderr, ocCheckToken) {
		t.Error("token in output")
	}
	if m, ok := opencode.LoadCheckCache("ocfake"); !ok || len(m) != 5 {
		t.Errorf("cache load = %v %v", m, ok)
	}
	if _, ok := opencode.LoadCheckCache("other"); ok {
		t.Error("cache must be scoped to its provider")
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

func TestOpencodeCheckExplicitIDsAndAll(t *testing.T) {
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
	if code, _, _, stderr := ocCheckJSON(t, "--all", "glm-5-2"); code != 1 || stderr == "" {
		t.Errorf("--all with ids: code=%d stderr=%q", code, stderr)
	}
	code, rows, _, _ = ocCheckJSON(t, "--all", "--timeout", "30ms")
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
}

func TestOpencodeListLiveCheckColumn(t *testing.T) {
	ocCheckEnv(t)
	err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "G"}, {ID: "kimi-k3", Label: "K"},
		{ID: "boom-1", Label: "B"}, {ID: "ghost", Label: "Ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	listJSON := func() map[string]string {
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
	// negative control: no cache yet
	for id, c := range listJSON() {
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
	want := map[string]string{"glm-5-2": "ok tools", "kimi-k3": "ok no-tools", "boom-1": "fail 500", "ghost": "missing"}
	for id, c := range listJSON() {
		if c != want[id] {
			t.Errorf("%s check = %q, want %q", id, c, want[id])
		}
	}
	// a cache for another provider must not show
	if err := opencode.SaveCheckCache(opencode.CheckCache{Provider: "other",
		Results: []serving.CheckResult{{ID: "glm-5-2", Status: "ok", Tools: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	if c := listJSON()["glm-5-2"]; c != "-" {
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

func syncEnv(t *testing.T, probe ...http.HandlerFunc) *syncFake {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DATABRICKS_SYNCTEST_PAT", "")
	f := &syncFake{}
	f.meCode.Store(200)
	f.listCode.Store(200)
	f.listing.Store(syncListing)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-sync-credential" ||
			(strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet) {
			t.Error("expected vault credential and GET for discovery/identity")
		}
		switch r.URL.Path {
		case "/api/2.0/preview/scim/v2/Me":
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
	}))
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

func TestOpencodeMenuSyncPreviewApplyFiltersAndIdempotence(t *testing.T) {
	f := syncEnv(t)
	existing := []providers.OpencodeModel{
		{ID: "retained-absent", Label: "Keep first", Responses: true},
		{ID: "gpt-5-5", Label: "Explicit chat override", Responses: false},
		{ID: "glm-5-2", Label: "Explicit Responses override", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
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
	sealed := syncVaultBytes(t)
	out := syncRun(t, "--apply")
	if f.me.Load() != 4 || f.list.Load() != 3 || !strings.Contains(out, "encrypted cache hit") || !bytes.Equal(sealed, syncVaultBytes(t)) {
		t.Fatalf("unchanged apply must authenticate, not list or write: Me=%d listing=%d out=%s", f.me.Load(), f.list.Load(), out)
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
			before := syncVaultBytes(t)
			syncRun(t, "--dry-run")
			if !bytes.Equal(before, syncVaultBytes(t)) {
				t.Fatal("no-additions preview wrote cache")
			}
			out := syncRun(t, "--apply")
			if !strings.Contains(out, "no menu change") || providers.OpencodeMenuFromVault() {
				t.Fatalf("empty/no-change discovery must not write menu: %s", out)
			}
			sealed := syncVaultBytes(t)
			if bytes.Equal(before, sealed) {
				t.Fatal("apply failed to persist discovery with zero additions")
			}
			syncRun(t, "--apply")
			if f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(sealed, syncVaultBytes(t)) {
				t.Fatalf("zero-addition cache hit rewrote or listed: Me=%d listing=%d", f.me.Load(), f.list.Load())
			}
			syncRun(t, "--refresh", "--apply")
			refreshed := syncVaultBytes(t)
			if f.me.Load() != 4 || f.list.Load() != 3 || bytes.Equal(sealed, refreshed) || providers.OpencodeMenuFromVault() {
				t.Fatal("zero-addition refresh must update discovery, not the menu")
			}
			syncRun(t, "--apply")
			if f.me.Load() != 5 || f.list.Load() != 3 || !bytes.Equal(refreshed, syncVaultBytes(t)) {
				t.Fatal("refreshed snapshot was not reused without writes")
			}
		})
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
		before := syncVaultBytes(t)
		if code, _, _ := ocMenu(t, args...); code == 0 || f.me.Load() != 0 || f.list.Load() != 0 || !bytes.Equal(before, syncVaultBytes(t)) {
			t.Fatalf("invalid controls reached HTTP or wrote state: %v, code=%d", args, code)
		}
	}
	if err := providers.SetProviderFields("synctest", map[string]any{"options": map[string]any{"wire": "ai-gateway"}}); err != nil {
		t.Fatal(err)
	}
	before := syncVaultBytes(t)
	if code, _, _ := ocMenu(t, "sync", "--profile", "synctest", "--apply"); code == 0 || f.me.Load() != 0 || f.list.Load() != 0 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatal("gateway profile was not rejected before HTTP/writes")
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
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") || strings.Contains(stdout+stderr, "private-upstream-body") {
		t.Fatal("identity error leaked sensitive text")
	}
	f.meCode.Store(200)
	f.listCode.Store(503)
	f.listing.Store("private-upstream-body synthetic-sync-credential")
	code, stdout, stderr = ocMenu(t, "sync", "--profile", "synctest", "--refresh", "--apply")
	if code == 0 || f.me.Load() != 3 || f.list.Load() != 2 || !bytes.Equal(before, syncVaultBytes(t)) {
		t.Fatalf("failed refresh used stale data or wrote state: code=%d Me=%d listing=%d", code, f.me.Load(), f.list.Load())
	}
	if strings.Contains(stdout+stderr, "synthetic-sync-credential") || strings.Contains(stdout+stderr, "private-upstream-body") {
		t.Fatal("listing error leaked sensitive text")
	}
}

func TestOpencodeMenuSyncHistoricalFailuresOnlyBlockNewEntries(t *testing.T) {
	f := syncEnv(t)
	existing := []providers.OpencodeModel{{ID: "gpt-5-5", Label: "Keep failed existing", Responses: false}}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
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
	out := syncRun(t, "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 3 || got[0] != existing[0] || slices.Contains(menuIDs(), "sync-alpha") ||
		!strings.Contains(out, "historical probes") || !strings.Contains(out, "1 failed additions skipped") {
		t.Fatalf("historical checks must block only failed NEW entries: menu=%+v out=%s", got, out)
	}
	if f.me.Load() != 2 || f.list.Load() != 1 || strings.Contains(out, "private-upstream-body") {
		t.Fatal("historical cache was not reused safely")
	}
	if _, err := os.Stat(opencode.CheckCachePath()); !os.IsNotExist(err) {
		t.Fatalf("sync touched legacy plaintext check cache: %v", err)
	}
}

func TestOpencodeMenuSyncVerifySealsSafeChecksAndPreservesExisting(t *testing.T) {
	var chat, responses atomic.Int32
	f := syncEnv(t, func(w http.ResponseWriter, r *http.Request) {
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
	})
	f.listing.Store(`{"endpoints":[
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-6-astra","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-sync-rejected","task":"llm/v1/chat","state":{"ready":"READY"}}]}`)
	existing := []providers.OpencodeModel{
		{ID: "gpt-5-5", Label: "Keep explicit chat flag", Responses: false},
		{ID: "absent-from-discovery", Label: "Keep absent", Responses: true},
	}
	if err := providers.SetOpencodeMenu(existing); err != nil {
		t.Fatal(err)
	}
	before := syncVaultBytes(t)
	preview := syncRun(t, "--verify", "--dry-run")
	if !bytes.Equal(before, syncVaultBytes(t)) || chat.Load() != 4 || responses.Load() != 1 {
		t.Fatal("verification preview must probe without persisting results or menu")
	}
	out := syncRun(t, "--verify", "--apply")
	got := providers.OpencodeMenu()
	if len(got) != 3 || !reflect.DeepEqual(got[:2], existing) || got[2].ID != "gpt-6-astra" || !got[2].Responses ||
		!strings.Contains(out, "1 failed additions skipped") || !strings.Contains(out, "fresh probes") {
		t.Fatalf("verified merge failed: menu=%+v output=%s", got, out)
	}
	if f.me.Load() != 2 || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatalf("wrong verification requests: Me=%d listing=%d chat=%d responses=%d", f.me.Load(), f.list.Load(), chat.Load(), responses.Load())
	}
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
	// Inspect decrypted storage too: read-time sanitation alone must not hide a persisted raw body.
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
	for _, forbidden := range []string{"private-probe-body", "synthetic-sync-credential"} {
		if strings.Contains(raw+preview+out, forbidden) {
			t.Error("verification persisted or printed private probe diagnostics")
		}
	}
	if _, err := os.Stat(opencode.CheckCachePath()); !os.IsNotExist(err) {
		t.Fatalf("verification touched legacy plaintext cache: %v", err)
	}
	sealed := syncVaultBytes(t)
	syncRun(t, "--apply")
	if !bytes.Equal(sealed, syncVaultBytes(t)) || f.list.Load() != 2 || chat.Load() != 8 || responses.Load() != 2 {
		t.Fatal("historical verified hit must not reprobe, relist, or rewrite")
	}
}
