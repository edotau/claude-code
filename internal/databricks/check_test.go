package databricks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/providers"
)

// probeServer is a TLS probe endpoint that holds each reply for delay and counts requests.
func probeServer(t *testing.T, delay time.Duration) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	old := probeClient
	probeClient = srv.Client()
	t.Cleanup(func() { probeClient = old })
	t.Setenv("DATABRICKS_TEST_PROBE_TOKEN", "test-token")
	for _, name := range []string{"p1", "p2", "p3"} {
		if err := providers.SetProviderFields(name, map[string]any{"preset": "databricks", "host": srv.URL,
			"auth": map[string]any{"type": "key", "env": "DATABRICKS_TEST_PROBE_TOKEN"}}); err != nil {
			t.Fatal(err)
		}
	}
	return &n
}

// Profiles and their wires probe concurrently, and rows keep the requested order.
func TestCheckProfilesConcurrentInOrder(t *testing.T) {
	seedOverlay(t, "")
	const delay = 300 * time.Millisecond
	n := probeServer(t, delay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"p3", "p1", "p2"}
	start := time.Now()
	rows := checkProfiles(reg, names, "claude-haiku-5-5", []string{providers.WireServing, providers.WireAIGateway})
	if took := time.Since(start); took >= 2*delay || n.Load() != 6 {
		t.Fatalf("3 profiles x 2 wires took %s with %d probes; serial would be %s", took, n.Load(), 6*delay)
	}
	for i, r := range rows {
		if len(r.cells) != 2 || r.cells[0] != "ok" || r.cells[1] != "ok" {
			t.Errorf("row %d (%s) = %+v", i, names[i], r)
		}
	}
}

// Without --wire only the serving wire is probed and the header names only it; a bad --wire is refused.
func TestCheckAllDefaultsToServing(t *testing.T) {
	seedOverlay(t, "")
	n := probeServer(t, 0)
	code, out, _ := runCaptured(t, func() int { return Run([]string{"check", "--all", "--profile", "p1"}) })
	if code != 0 || n.Load() != 1 || !strings.Contains(out, "SERVING") || strings.Contains(out, "AI-GATEWAY") {
		t.Fatalf("exit = %d, probes = %d, stdout = %q", code, n.Load(), out)
	}
	if code, _, _ := runCaptured(t, func() int { return Run([]string{"check", "--all", "--wire", "bogus"}) }); code == 0 {
		t.Fatal("--wire bogus accepted")
	}
}

// A check's token request reports a 429 after one attempt; it never waits out the dispatch retries.
func TestCheckProfileTokenNoRetry(t *testing.T) {
	seedOverlay(t, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	oldBackoff := oauth.RetryBackoff
	oauth.RetryBackoff = time.Millisecond
	t.Cleanup(func() { oauth.RetryBackoff = oldBackoff })
	t.Setenv("DATABRICKS_NORETRY_M2M", "client-secret")
	row := &providers.Provider{Name: "noretry", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: srv.URL + "/serving-endpoints/anthropic", Auth: providers.Auth{Type: providers.AuthM2M, ClientID: "c"}}
	r := checkProfile(row, "noretry", "m", []string{providers.WireServing})
	if r.cells[0] != "no token" || calls.Load() != 1 {
		t.Fatalf("cells = %q, token calls = %d, want no token after 1 call", r.cells, calls.Load())
	}
	if _, err := oauth.MintM2M(context.Background(), srv.URL, "c", "client-secret"); err == nil || calls.Load() != 4 {
		t.Fatalf("a dispatch mint must keep its retries: calls = %d", calls.Load())
	}
}
