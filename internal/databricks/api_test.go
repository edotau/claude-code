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

func TestAPITargetValidation(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"/api/2.0/serving-endpoints", "/api/2.0/serving-endpoints"},
		{"/api/2.0/serving-endpoints/", "/api/2.0/serving-endpoints"},
		{"/api/2.0/serving-endpoints/my-ep?limit=1", "/api/2.0/serving-endpoints/my-ep?limit=1"},
		{"/api/2.0/sql/statements/abc", "/api/2.0/sql/statements/abc"},
		{"/api/2.0/preview/scim/v2/Me", "/api/2.0/preview/scim/v2/Me"},
		{"/api/2.1/unity-catalog/metastores/m1/systemschemas", "/api/2.1/unity-catalog/metastores/m1/systemschemas"},
		{"https://evil.example/x", ""},
		{"//evil.example/api/x", ""},
		{"/api/../x", ""},
		{"/api/2.0/sql/warehouses/../x", ""},
		{"/api/2.0/serving-endpoints/%2e%2e/x", ""},
		{"/api/2.0/serving-endpoints/%252e%252e", ""},
		{"/api/2.0/serving-endpoints/./x", "/api/2.0/serving-endpoints/x"},
		{"/api/2.0/serving-endpoints/%252e%252e/x", ""},
		{"/api/2.0/serving-endpoints/a%5cb", ""},
		{"/api/2.0/serving-endpoints/a%3Fb", ""},
		{"/api/2.0/serving-endpoints/x y", ""},
		{"/api/2.0/serving-endpoints/x\ny", ""},
		{"/api/2.0/serving-endpoints?a=%41", ""},
		{"/api/2.0/serving-endpoints?a=b c", ""},
		{"/api/2.0/serving-endpoints-evil", ""},
		{"/api/2.0/serving-endpoints/a/config", ""},
		{"/api/2.0/workspace/list?path=/", ""},
		{"/api/2.0/preview/scim/v2/Users", ""},
		{"/other/api/x", ""},
	}
	for _, c := range cases {
		got, err := apiTarget(c.raw)
		if (err == nil) != (c.want != "") || got != c.want {
			t.Errorf("apiTarget(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
	if _, err := apiTarget("/api/2.0/workspace/list"); err == nil || !strings.Contains(err.Error(), "/api/2.0/sql/warehouses") {
		t.Errorf("refusal must list the allowed prefixes: %v", err)
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
		if r.Method != http.MethodGet || r.URL.Path != "/api/2.0/serving-endpoints/x" || r.Header.Get("Authorization") != "Bearer synthetic-api-token" {
			t.Errorf("request = %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"a":1}`))
	})
	code, out, errs := runAPI(t, "/api/2.0/serving-endpoints/x")
	if code != 0 || out != "{\n  \"a\": 1\n}\n" || strings.Contains(out+errs, "synthetic-api-token") {
		t.Errorf("api = %d %q %q", code, out, errs)
	}
}

func TestAPINon2xxExitsNonZeroWithStatus(t *testing.T) {
	apiServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no such thing", http.StatusNotFound) })
	code, out, errs := runAPI(t, "/api/2.0/serving-endpoints/x")
	if code != 1 || out != "" || !strings.Contains(errs, "HTTP 404: no such thing") {
		t.Errorf("api = %d %q %q", code, out, errs)
	}
}

func TestAPIRefusesNonDatabricksProfile(t *testing.T) {
	var hits atomic.Int32
	apiServer(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	if err := providers.SetProviderFields("other-key", map[string]any{"protocol": "openai", "base_url": "https://other.invalid/v1",
		"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_API_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"api", "/api/2.0/serving-endpoints", "--profile", "other-key"})
	})
	if code != 2 || out != "" || hits.Load() != 0 || !strings.Contains(errs, "not a Databricks profile") || !strings.Contains(errs, "api-test") {
		t.Errorf("api = %d %q %q hits %d", code, out, errs, hits.Load())
	}
}

func TestAPIRefusesBadPathBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	apiServer(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	code, _, errs := runAPI(t, "//evil.example/api/x")
	if code != 2 || hits.Load() != 0 || !strings.Contains(errs, "path must be plain") {
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
	code, _, errs := runAPI(t, "/api/2.0/serving-endpoints/x")
	if code != 1 || second.Load() != 0 || !strings.Contains(errs, "HTTP 302") {
		t.Errorf("api = %d second host hits %d %q", code, second.Load(), errs)
	}
}
