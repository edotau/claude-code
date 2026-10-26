package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const dbxServingBody = `{"endpoints":[
  {"name":"databricks-claude-sonnet","task":"llm/v1/chat","state":{"ready":"READY"}},
  {"name":"embed-small","task":"llm/v1/embeddings","state":{"ready":"NOT_READY"}},
  {"name":"gpt-untasked","task":"","state":{"ready":"READY"}}]}`

// pathRecorder records every request path and the Authorization header the test server saw.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
	auth  []string
}

func (r *pathRecorder) hit(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
}

func (r *pathRecorder) saw(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if p == path {
			return true
		}
	}
	return false
}

// trustTestTLS lets http.DefaultClient (used by liveModelEntries) call an httptest TLS server.
func trustTestTLS(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// liveDbxProvider is a Databricks preset row whose Host is the test server; the bearer comes from an env var.
func liveDbxProvider(t *testing.T, host, token string) *Provider {
	t.Helper()
	const env = "DBX_LIVE_TEST_TOKEN"
	t.Setenv(env, token)
	return &Provider{Name: "dbx-live-test", Preset: PresetDatabricks, Host: host,
		Auth: Auth{Type: AuthBearer, Env: env}}
}

func TestListModelsDatabricksServingEndpoints(t *testing.T) {
	var rec pathRecorder
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hit(r)
		switch r.URL.Path {
		case "/api/2.0/serving-endpoints":
			writeJSON(w, 200, dbxServingBody)
		case "/v1/models":
			writeJSON(w, 200, `{"data":[{"id":"must-not-appear"}]}`)
		default:
			writeJSON(w, 404, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	trustTestTLS(t, srv)
	p := liveDbxProvider(t, srv.URL, "tok-abc123")

	got, err := ListModels(context.Background(), nil, p)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []struct{ id, caps, task, ready string }{
		{"databricks-claude-sonnet", "llm/v1/chat · READY", "llm/v1/chat", "READY"},
		{"embed-small", "llm/v1/embeddings · NOT_READY", "llm/v1/embeddings", "NOT_READY"},
		{"gpt-untasked", "READY", "", "READY"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Capabilities != w.caps || got[i].Task != w.task || got[i].Ready != w.ready {
			t.Errorf("row %d = %+v, want id=%q caps=%q task=%q ready=%q", i, got[i], w.id, w.caps, w.task, w.ready)
		}
	}
	if !rec.saw("/api/2.0/serving-endpoints") {
		t.Errorf("serving-endpoints not hit; paths = %v", rec.paths)
	}
	if rec.saw("/v1/models") {
		t.Errorf("Databricks provider hit /v1/models; paths = %v", rec.paths)
	}
	for _, a := range rec.auth {
		if a != "Bearer tok-abc123" {
			t.Errorf("Authorization = %q, want %q", a, "Bearer tok-abc123")
		}
	}
}

func TestLiveModelsDatabricksIDsOnly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, dbxServingBody)
	}))
	t.Cleanup(srv.Close)
	trustTestTLS(t, srv)
	ids, err := LiveModels(context.Background(), liveDbxProvider(t, srv.URL, "tok-ids"))
	if err != nil {
		t.Fatalf("LiveModels: %v", err)
	}
	if strings.Join(ids, ",") != "databricks-claude-sonnet,embed-small,gpt-untasked" {
		t.Errorf("ids = %v", ids)
	}
}

func TestListModelsNonDatabricksUsesV1Models(t *testing.T) {
	cases := []struct {
		name, kind, path string
	}{
		{"anthropic", DialectAnthropic, "/v1/models"},
		{"openai", DialectOpenAI, "/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec pathRecorder
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.hit(r)
				if r.URL.Path == tc.path {
					writeJSON(w, 200, `{"data":[{"id":"plain-model","display_name":"Plain Model"}]}`)
					return
				}
				writeJSON(w, 200, dbxServingBody) // must never be parsed for a non-Databricks row
			}))
			t.Cleanup(srv.Close)
			t.Setenv("PLAIN_LIVE_TEST_TOKEN", "tok-plain")
			p := &Provider{Name: "plain-live-test", Kind: tc.kind, BaseURL: srv.URL,
				Auth: Auth{Type: AuthBearer, Env: "PLAIN_LIVE_TEST_TOKEN"}}

			got, err := ListModels(context.Background(), nil, p)
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if len(got) != 1 || got[0].ID != "plain-model" || got[0].Name != "Plain Model" || got[0].Capabilities != "" {
				t.Errorf("got %+v, want one plain-model row with no capabilities", got)
			}
			if !rec.saw(tc.path) {
				t.Errorf("%s not hit; paths = %v", tc.path, rec.paths)
			}
			if rec.saw("/api/2.0/serving-endpoints") {
				t.Errorf("non-Databricks provider hit serving-endpoints; paths = %v", rec.paths)
			}
		})
	}
}

func TestListModelsNon200DoesNotLeakBearer(t *testing.T) {
	const secret = "tok-must-not-leak-42"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, `{"error_code":"PERMISSION_DENIED","message":"no access to serving endpoints"}`)
	}))
	t.Cleanup(srv.Close)
	trustTestTLS(t, srv)

	_, err := ListModels(context.Background(), nil, liveDbxProvider(t, srv.URL, secret))
	if err == nil {
		t.Fatal("ListModels on 403 returned nil error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q does not name the status", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the bearer token: %q", err)
	}
}
