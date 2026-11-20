package databricks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

func TestAPIPathValidation(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
	}{
		{"/api/2.0/serving-endpoints", true},
		{"/api/2.0/serving-endpoints?limit=1", true},
		{"https://evil.example/x", false},
		{"//evil.example/api/x", false},
		{"/api/../x", false},
		{"/api/%2e%2e/x", false},
		{"/api/x y", false},
		{"/api/x\ny", false},
		{"/api/https://evil.example", false},
		{"/other/api/x", false},
		{"api/2.0/x", false},
	}
	for _, c := range cases {
		if got := apiPath(c.path) == ""; got != c.ok {
			t.Errorf("apiPath(%q) accepted = %v, want %v", c.path, got, c.ok)
		}
	}
}

// apiServer is a TLS workspace answering h, with a profile "api-test" on it and apiClient trusting it.
func apiServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	seedOverlay(t, "")
	t.Setenv("DATABRICKS_TEST_API_TOKEN", "synthetic-api-token")
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	old := apiClient
	apiClient = srv.Client()
	t.Cleanup(func() { apiClient = old })
	if err := providers.SetProviderFields("api-test", map[string]any{"preset": "databricks", "host": srv.URL,
		"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_API_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
}

func runAPI(t *testing.T, path string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return Run([]string{"api", path, "--profile", "api-test"}) })
}

func TestAPIGetPrettyPrintsAndHidesToken(t *testing.T) {
	apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/2.0/x" || r.Header.Get("Authorization") != "Bearer synthetic-api-token" {
			t.Errorf("request = %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	})
	code, out, errs := runAPI(t, "/api/2.0/x")
	if code != 0 || out != "{\n  \"a\": 1\n}\n" || strings.Contains(out+errs, "synthetic-api-token") {
		t.Errorf("api = %d %q %q", code, out, errs)
	}
}

func TestAPINon2xxExitsNonZeroWithStatus(t *testing.T) {
	apiServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such thing", http.StatusNotFound) })
	code, out, errs := runAPI(t, "/api/2.0/x")
	if code != 1 || out != "" || !strings.Contains(errs, "HTTP 404: no such thing") {
		t.Errorf("api = %d %q %q", code, out, errs)
	}
}

func TestAPIRefusesBadPathBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	apiServer(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	code, _, errs := runAPI(t, "//evil.example/api/x")
	if code != 2 || hits.Load() != 0 || !strings.Contains(errs, "scheme or host") {
		t.Errorf("api = %d hits %d %q", code, hits.Load(), errs)
	}
}

func TestAPIDoesNotFollowRedirect(t *testing.T) {
	var second atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization reached a second host")
		}
	}))
	t.Cleanup(other.Close)
	apiServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/x", http.StatusFound) })
	code, _, errs := runAPI(t, "/api/2.0/x")
	if code != 1 || second.Load() != 0 || !strings.Contains(errs, "HTTP 302") {
		t.Errorf("api = %d second host hits %d %q", code, second.Load(), errs)
	}
}
