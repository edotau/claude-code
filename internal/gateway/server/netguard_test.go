package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// rejectLocal must stop every non-public resolved address and pass a public one.
func TestRejectLocal(t *testing.T) {
	for addr, wantErr := range map[string]bool{
		"127.0.0.1:80": true, "10.1.2.3:443": true, "192.168.1.1:443": true, "172.16.0.9:443": true,
		"169.254.169.254:80": true, "[::1]:443": true, "0.0.0.0:80": true, "8.8.8.8:443": false, "[2606:4700::1]:443": false,
	} {
		err := rejectLocal("tcp", addr, nil)
		if (err != nil) != wantErr {
			t.Errorf("rejectLocal(%s): err=%v, want error=%v", addr, err, wantErr)
		}
	}
}

// A literal loopback URL is allowed; a hostname resolving to the same address is refused.
func TestGuardedClientLiteralVsResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	c := guardedClient()
	resp, err := c.Do(testRequest(t, http.MethodGet, srv.URL, nil))
	if err != nil {
		t.Fatalf("literal loopback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 204 {
		t.Fatalf("literal loopback: status=%d", resp.StatusCode)
	}
	for _, host := range []string{"localhost", "api.localhost"} {
		if !literalLocal(host) {
			t.Errorf("literalLocal(%q) = false", host)
		}
	}
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	localResp, err := c.Do(
		testRequest(t, http.MethodGet, "http://localhost.:"+port, nil),
	) // trailing dot is still literal
	if err != nil {
		t.Errorf("localhost. must dial: %v", err)
	} else {
		_ = localResp.Body.Close()
	}
	// The guarded dialer is what a non-literal name reaches; its Control refuses the loopback it resolved to.
	_, err = guardedDialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil || !strings.Contains(err.Error(), "resolves into the local network") {
		t.Errorf("guarded dialer must refuse a loopback target: %v", err)
	}
}

// resolveProxyHosts records the host of each scheme's proxy; the env lookup is stubbed because net/http caches it.
func TestResolveProxyHosts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy map[string]string // request scheme -> the proxy URL the environment names for it
		want  string
	}{
		{"https proxy host recorded", map[string]string{"https": "http://proxy.corp:3128"}, "true 1"},
		{"no proxy configured", nil, "false 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := proxyFromEnvironment
			proxyFromEnvironment = func(req *http.Request) (*url.URL, error) {
				raw := tc.proxy[req.URL.Scheme]
				if raw == "" {
					return nil, nil
				}
				return url.Parse(raw)
			}
			t.Cleanup(func() { proxyFromEnvironment = old })
			hosts := resolveProxyHosts()
			if got := fmt.Sprint(hosts["proxy.corp"], " ", len(hosts)); got != tc.want {
				t.Errorf("resolveProxyHosts() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Configured proxy hosts may resolve locally; other upstream hostnames may not.
func TestGuardedDialProxyHostVsBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	port := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")

	old := proxyHosts
	proxyHosts = map[string]bool{"127.0.0.1": true}
	t.Cleanup(func() { proxyHosts = old })

	conn, err := guardedDial(context.Background(), "tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("configured proxy host must dial unguarded: %v", err)
	}
	_ = conn.Close()

	// Regression guard: a non-proxy host resolving to loopback is still refused, proxyHosts notwithstanding.
	proxyHosts = map[string]bool{"proxy.corp": true}
	_, err = guardedDialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil || !strings.Contains(err.Error(), "resolves into the local network") {
		t.Errorf("non-proxy host resolving to loopback must still be refused: %v", err)
	}
}

// A header-stalled upstream must fail the attempt (so failover can proceed) instead of hanging.
func TestGuardedClientHeaderStallTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := guardedClient()
	tr := c.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != responseHeaderTimeout || responseHeaderTimeout <= 0 {
		t.Fatalf("ResponseHeaderTimeout=%v, want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	tr.ResponseHeaderTimeout = 100 * time.Millisecond
	resp, err := c.Do(testRequest(t, http.MethodGet, srv.URL, nil))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("stalled upstream returned a response, want a header timeout error")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Errorf("err=%v, want header timeout", err)
	}
}

// A /via/<host>/… request is authenticated, routed as the bare path, and its upstream request carries the host.
func TestViaPrefixPinsEgressHost(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`)
	var pinned atomic.Value
	var log bytes.Buffer
	r := newRouter(secret)
	r.log = &log
	r.client = &http.Client{Transport: &http.Transport{Proxy: func(req *http.Request) (*url.URL, error) {
		pinned.Store(egress.PinnedHost(req.Context()))
		return nil, nil
	}}}
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); r.bg.Wait() })

	if resp := post(t, srv.URL+"/via/box/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("via request: status %d", resp.StatusCode)
	}
	if got := up.last.Load().URL.Path; got != "/v1/messages" || pinned.Load() != "box" {
		t.Errorf("upstream path %q, pinned host %v; want /v1/messages and box", got, pinned.Load())
	}
	if resp := post(t, srv.URL+"/via/box/p/up1/v1/messages", `{"model":"m1"}`, map[string]string{"X-Api-Key": ""}); resp.StatusCode != 401 {
		t.Errorf("via request without the secret: status %d, want 401", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/via/-oProxyCommand=x/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 400 {
		t.Errorf("bad via host: status %d, want 400", resp.StatusCode)
	}
	if up.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", up.hits.Load())
	}
	srv.Close() // handlers are done: the log is safe to read
	if !strings.Contains(log.String(), " via=box") {
		t.Errorf("log line lacks via=box: %q", log.String())
	}
}
