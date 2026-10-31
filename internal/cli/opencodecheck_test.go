package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
			_, _ = w.Write([]byte(`{"endpoints":[
{"name":"databricks-glm-5-2","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-kimi-k3","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-boom-1","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-slow-1","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-embed-1","task":"llm/v1/embeddings","state":{"ready":"READY"}}]}`))
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
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	err := providers.SetProviderFields("ocfake", map[string]any{"preset": "databricks", "host": srv.URL,
		"auth": map[string]any{"type": "pat"}})
	if err != nil {
		t.Fatal(err)
	}
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
	cache := agent.CheckCachePath()
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
	if m, ok := agent.LoadCheckCache("ocfake"); !ok || len(m) != 5 {
		t.Errorf("cache load = %v %v", m, ok)
	}
	if _, ok := agent.LoadCheckCache("other"); ok {
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

func TestOpencodeCheckSortOrder(t *testing.T) {
	rs := []agent.CheckResult{
		{ID: "kimi-k3", Status: "ok", LatencyMS: 5}, {ID: "glm-5-2", Status: "fail", LatencyMS: 1},
		{ID: "glm-5-3", Status: "ok", LatencyMS: 900}, {ID: "glm-5-1", Status: "ok", LatencyMS: 100},
		{ID: "claude-x", Status: "ok", LatencyMS: 50},
	}
	agent.SortCheckResults(rs)
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	if want := "claude-x glm-5-1 glm-5-3 glm-5-2 kimi-k3"; strings.Join(ids, " ") != want {
		t.Errorf("order = %v, want %s", ids, want)
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
	if err := agent.SaveCheckCache(agent.CheckCache{Provider: "other",
		Results: []agent.CheckResult{{ID: "glm-5-2", Status: "ok", Tools: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	if c := listJSON()["glm-5-2"]; c != "-" {
		t.Errorf("foreign cache shown: %q", c)
	}
}

func TestOpencodeCheckFamily(t *testing.T) {
	for in, want := range map[string]string{"claude-opus-5-5": "claude", "databricks-glm-5-2": "glm", "gpt-oss-120b": "gpt", "inkling": "inkling"} {
		if got := agent.CheckFamily(in); got != want {
			t.Errorf("CheckFamily(%q) = %q, want %q", in, got, want)
		}
	}
}
