package opencode

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
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
	return captureOutput(t, func() int { return RunMenu(args) })
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
	ocCheckEnv(t)
	if code, _, _ := ocMenu(t, "add", "--profile", "ocfake", "--force", "bogus-model=Bogus"); code != 0 {
		t.Fatal("--force must bypass the workspace check")
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

func TestOpencodeMenuImportJSONReplacesMenu(t *testing.T) {
	ocCheckEnv(t)
	doc := `{"provider":{"databricks-qa":{"models":{"databricks-kimi-k3":{"name":"Kimi K3 · qa"}}}}}`
	cfg := writeImportFile(t, "opencode.json", doc)
	if code, _, stderr := ocMenu(t, "import", "--apply", cfg); code != 0 {
		t.Fatalf("json apply: %d %s", code, stderr)
	}
	if got := menuIDs(); len(got) != 1 || got[0] != "kimi-k3" {
		t.Errorf("import must replace the whole menu, got %v", got)
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
	for _, verb := range menuVerbs {
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
	cache := CheckCachePath()
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
	if m, ok := LoadCheckCache("ocfake"); !ok || len(m) != 5 {
		t.Errorf("cache load = %v %v", m, ok)
	}
	if _, ok := LoadCheckCache("other"); ok {
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

func TestOpencodeCheckAllRejectsIDs(t *testing.T) {
	ocCheckEnv(t)
	if code, _, _, stderr := ocCheckJSON(t, "--all", "glm-5-2"); code != 1 || stderr == "" {
		t.Errorf("--all with ids: code=%d stderr=%q", code, stderr)
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
	for id, c := range liveCheckColumns(t) {
		if c != want[id] {
			t.Errorf("%s check = %q, want %q", id, c, want[id])
		}
	}
	// a cache for another provider must not show
	if err := SaveCheckCache(CheckCache{Provider: "other",
		Results: []serving.CheckResult{{ID: "glm-5-2", Status: "ok", Tools: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	if c := liveCheckColumns(t)["glm-5-2"]; c != "-" {
		t.Errorf("foreign cache shown: %q", c)
	}
}

// captureOutput redirects os.Stdout/os.Stderr around fn and returns fn's exit code plus both streams.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var drained sync.WaitGroup
	drained.Go(func() { io.Copy(&bo, ro) }) // drain while fn runs: a full pipe would block its writes forever
	drained.Go(func() { io.Copy(&be, re) })
	os.Stdout, os.Stderr = wo, we
	code = fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	drained.Wait()
	return code, bo.String(), be.String()
}

func useTLSTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
}
