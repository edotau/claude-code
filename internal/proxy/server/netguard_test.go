package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/remote/egress"
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

// A 3xx goes back to the caller; the redirect target is never dialed.
func TestGuardedClientNeverFollowsRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	first := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer first.Close()
	resp, err := guardedClient().Do(testRequest(t, http.MethodGet, first.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != target.URL || hits.Load() != 0 {
		t.Errorf("status %d, Location %q, target hits %d; want a redirect to %s with no hit", resp.StatusCode, resp.Header.Get("Location"), hits.Load(), target.URL)
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

func TestDoBoundedHeaderTimeoutOverride(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	r := newRouter("s")
	req := testRequest(t, http.MethodGet, srv.URL, nil)
	req = req.WithContext(withHeaderTimeout(req.Context(), 80*time.Millisecond))
	start := time.Now()
	resp, err := r.doBounded(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected fast header timeout error")
	}
	if time.Since(start) > 2*time.Second || !strings.Contains(err.Error(), "upstream sent no response within") {
		t.Fatalf("err=%v elapsed=%v, want fast header timeout", err, time.Since(start))
	}
}

// Finding 11: a repeated /d/ or /via/ prefix is a 400, never a silent replacement of the first.
func TestStripViaRejectsDuplicatePrefix(t *testing.T) {
	for _, path := range []string{"/d/a/d/b/p/x", "/via/h1/via/h2/p/x", "/d/a/via/h/d/b/p/x", "/via/h/d/a/via/h2/p/x"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, nil)
		if _, ok := stripVia(w, req); ok || w.Code != http.StatusBadRequest {
			t.Errorf("%s: ok=%t status=%d, want a 400", path, ok, w.Code)
		}
	}
}

// The dispatch tag is stripped only as a leading segment: a 128-char id passes, an empty or 129-char one is a 400.
func TestStripViaDispatchSegmentShapes(t *testing.T) {
	const maxID = 128 // the documented limit, deliberately not read from gateway
	id128 := strings.Repeat("a", maxID)
	for _, tc := range []struct {
		name, path, wantPath, wantDiag string
		wantCode                       int // 0 = passes
	}{
		{"128-char id", "/d/" + id128 + "/p/x", "/p/x", " dispatch=" + id128, 0},
		{"129-char id", "/d/" + id128 + "a/p/x", "", "", http.StatusBadRequest},
		{"empty segment", "/d//p/x", "", "", http.StatusBadRequest},
		{"no path after the id", "/d/abc", "/", " dispatch=abc", 0},
		{"tag after the profile segment is not a tag", "/p/up1/d/abc/v1/messages", "/p/up1/d/abc/v1/messages", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			out, ok := stripVia(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+tc.path, nil))
			if ok != (tc.wantCode == 0) || tc.wantCode != 0 && w.Code != tc.wantCode {
				t.Fatalf("ok=%t status=%d, want status %d", ok, w.Code, tc.wantCode)
			}
			if ok && (out.URL.Path != tc.wantPath || dispatchDiag(out) != tc.wantDiag) {
				t.Errorf("path %q diag %q, want %q %q", out.URL.Path, dispatchDiag(out), tc.wantPath, tc.wantDiag)
			}
		})
	}
}

// /d/<id>/ composes with /via/<host>/ in either order, logs dispatch=<id>, and a bad id is a 400 that never routes.
func TestDispatchPrefixLogsDispatchID(t *testing.T) {
	up := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"default":"up1","providers":{"anthropic":{"up1":`+anthropicProvider(up.URL, "UP1_KEY", `{"opus":"m1"}`)+`}}}`)
	var log bytes.Buffer
	r := newRouter(secret)
	r.log = &log
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); r.bg.Wait() })

	for _, prefix := range []string{"/d/20261113T144909.123-codex", "/via/box/d/a.b-1T", "/d/a.b-1T/via/box"} {
		if resp := post(t, srv.URL+prefix+"/p/up1/v1/messages", `{"model":"m1"}`, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", prefix, resp.StatusCode)
		}
	}
	for _, bad := range []string{"/d//p/up1/v1/messages", "/d/a%20b/p/up1/v1/messages", "/d/a_b/p/up1/v1/messages", "/d/" + strings.Repeat("a", 129) + "/p/up1/v1/messages"} {
		if resp := post(t, srv.URL+bad, `{"model":"m1"}`, nil); resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", bad, resp.StatusCode)
		}
	}
	if up.hits.Load() != 3 {
		t.Errorf("upstream hits = %d, want 3 (a bad id never routes)", up.hits.Load())
	}
	srv.Close() // handlers are done: the log is safe to read
	for _, want := range []string{" dispatch=20261113T144909.123-codex", " via=box dispatch=a.b-1T\n"} {
		if strings.Count(log.String(), want) < 1 {
			t.Errorf("log lacks %q: %q", want, log.String())
		}
	}
	if n := strings.Count(log.String(), " via=box dispatch=a.b-1T"); n != 2 {
		t.Errorf("both compose orders should log via=box dispatch=a.b-1T, got %d: %q", n, log.String())
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

// BenchmarkGuardedClientReconnect times a fresh connection to a local TLS server; the first request primes the cache.
func BenchmarkGuardedClientReconnect(b *testing.B) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b.Cleanup(srv.Close)
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cache=%t", cached), func(b *testing.B) {
			c := guardedClient()
			tr := c.Transport.(*http.Transport)
			tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			if !cached {
				tr.TLSClientConfig.ClientSessionCache = nil
			}
			var resumed int
			for i := 0; b.Loop(); i++ {
				resp, err := c.Get(srv.URL)
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if i > 0 && resp.TLS.DidResume {
					resumed++
				}
				tr.CloseIdleConnections()
			}
			if cached && b.N > 1 && resumed != b.N-1 {
				b.Fatalf("resumed %d of %d reconnects", resumed, b.N-1)
			}
		})
	}
}

// The upstream transport carries the HTTP/2 health-ping timings.
func TestGuardedClientHTTP2Pings(t *testing.T) {
	h2 := guardedClient().Transport.(*http.Transport).HTTP2
	if h2 == nil || h2.SendPingTimeout != 30*time.Second || h2.PingTimeout != 10*time.Second {
		t.Fatalf("HTTP2 = %+v, want SendPingTimeout 30s and PingTimeout 10s", h2)
	}
}

// Pre-warm turns the first request's full TLS handshake into a resumed one; no request is sent.
func TestPrewarmResumesFirstRequest(t *testing.T) {
	var requests, full, resumed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(204)
	}))
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	addr := srv.Listener.Addr().String()
	for _, warm := range []bool{false, true} {
		requests.Store(0)
		full.Store(0)
		resumed.Store(0)
		c := guardedClient()
		tr := c.Transport.(*http.Transport)
		tr.TLSClientConfig.RootCAs = pool
		tr.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			if cs.DidResume {
				resumed.Add(1)
			} else {
				full.Add(1)
			}
			return nil
		}
		if warm {
			warmHosts(context.Background(), tr, []string{addr})
		}
		start := time.Now()
		resp, err := c.Do(testRequest(t, http.MethodGet, srv.URL, nil))
		if err != nil {
			t.Fatalf("warm=%v: %v", warm, err)
		}
		_ = resp.Body.Close()
		t.Logf("warm=%v first request %s: full handshakes=%d resumed=%d", warm, time.Since(start), full.Load(), resumed.Load())
		if wantResumed := int32(0); warm {
			wantResumed = 1
			if resumed.Load() != wantResumed || full.Load() != 1 || requests.Load() != 1 {
				t.Errorf("warm: full=%d resumed=%d requests=%d, want full 1, resumed 1", full.Load(), resumed.Load(), requests.Load())
			}
		} else if resumed.Load() != 0 {
			t.Errorf("cold: resumed=%d, want 0", resumed.Load())
		}
	}
}

// A dead upstream fails the pre-warm silently and within its budget.
func TestWarmHostsFailsSilently(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), prewarmBudget)
	defer cancel()
	warmHosts(ctx, guardedClient().Transport.(*http.Transport), []string{addr, "bad"})
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(ctx.Err())
	}
}
