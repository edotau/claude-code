package server

import (
	"cmp"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/remote/egress"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// guardedClient rejects hostnames resolving locally; configured literal IPs and localhost remain allowed.
func guardedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = guardedDial
	t.Proxy = egress.Proxy // a /via/<host> request's workspace calls leave through that host's tunnel
	t.ResponseHeaderTimeout = responseHeaderTimeout
	t.MaxIdleConnsPerHost = upstreamIdleConns
	t.TLSClientConfig = withSessionCache(t.TLSClientConfig)
	// a silently dropped idle conn is pinged after 30s and closed if silent 10s more, before a request stalls on it
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 10 * time.Second}
	// a 3xx goes back to the caller: following it would dial a literal local IP past the guard
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// withSessionCache clones cfg (nil is fine) with a session cache so a reconnect resumes TLS, not a full handshake.
func withSessionCache(cfg *tls.Config) *tls.Config {
	c := cfg.Clone()
	if c == nil {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	c.ClientSessionCache = tls.NewLRUClientSessionCache(64)
	return c
}

// prewarmBudget bounds the startup pre-warm; it runs in the background and fails silently.
const prewarmBudget = 2 * time.Second

// prewarm handshakes the Anthropic route of each recently used provider so its first request resumes TLS.
func (r *Router) prewarm(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, prewarmBudget)
	defer cancel()
	t, ok := r.client.Transport.(*http.Transport)
	reg, err := r.registry()
	if !ok || err != nil {
		return
	}
	var addrs []string
	for _, n := range r.recentlyUsed() {
		if p := reg.Providers[n]; p != nil {
			if addr, ok := upstreamAddr(p); ok {
				addrs = append(addrs, addr)
			}
		}
	}
	slices.Sort(addrs)
	warmHosts(ctx, t, slices.Compact(addrs))
}

// upstreamAddr is host:port of p's Anthropic route; false when the route is not https.
func upstreamAddr(p *providers.Provider) (string, bool) {
	raw, ok := p.Route(providers.ProtocolAnthropic)
	u, err := url.Parse(raw)
	if !ok || err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", false
	}
	return net.JoinHostPort(u.Hostname(), cmp.Or(u.Port(), "443")), true
}

// warmHosts handshakes each addr once and keeps only its session; no request leaves, proxied routes skip.
func warmHosts(ctx context.Context, t *http.Transport, addrs []string) {
	var wg sync.WaitGroup
	for _, addr := range addrs {
		if !proxied(t, addr) {
			wg.Go(func() { warmOne(ctx, t.TLSClientConfig, addr) })
		}
	}
	wg.Wait()
}

// proxied reports whether the transport would send addr through a proxy, or cannot tell (then skip).
func proxied(t *http.Transport, addr string) bool {
	if t.Proxy == nil {
		return false
	}
	u, err := t.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	return err != nil || u != nil
}

// warmOne handshakes addr with cfg's session cache; errors are ignored, the first request just pays the full handshake.
func warmOne(ctx context.Context, cfg *tls.Config, addr string) {
	conn, err := guardedDial(ctx, "tcp", addr)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	c := cfg.Clone()
	if c == nil {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	c.ServerName, _, _ = net.SplitHostPort(addr)
	c.NextProtos = []string{"h2", "http/1.1"}
	tc := tls.Client(conn, c)
	if tc.HandshakeContext(ctx) != nil {
		return
	}
	// TLS 1.3 session tickets arrive after the handshake and are read only on Read; drain briefly to cache them
	_ = tc.SetReadDeadline(time.Now().Add(warmTicketWait))
	var b [1]byte
	_, _ = tc.Read(b[:])
}

// warmTicketWait is how long a pre-warm waits for the server's session ticket after the handshake.
const warmTicketWait = 300 * time.Millisecond

// upstreamIdleConns is the idle connections kept per workspace host: Go's default of 2 re-handshakes TLS in parallel.
const upstreamIdleConns = 32

// responseHeaderTimeout bounds the wait for upstream response headers so a wedged upstream fails over.
const responseHeaderTimeout = 120 * time.Second

var (
	plainDialer   = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guardedDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: rejectLocal}
)

// guardedDial exempts configured proxy hosts and literal local addresses before DNS resolution.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil && literalLocal(host) && net.ParseIP(strings.Trim(host, "[]")) == nil {
		addr = net.JoinHostPort("127.0.0.1", port) // RFC 6761: *.localhost is loopback; never ask this machine's DNS
	}
	if err == nil && (literalLocal(host) || proxyHosts[strings.ToLower(host)]) {
		return plainDialer.DialContext(ctx, network, addr)
	}
	return guardedDialer.DialContext(ctx, network, addr)
}

// proxyHosts holds the hostnames of HTTP(S)_PROXY as seen by http.ProxyFromEnvironment, resolved once at init.
var proxyHosts = resolveProxyHosts()

// proxyFromEnvironment is net/http's env lookup, which caches the env for the process; tests swap it.
var proxyFromEnvironment = http.ProxyFromEnvironment

// resolveProxyHosts reads the proxy env once; net/http caches those values, so later env changes are ignored.
func resolveProxyHosts() map[string]bool {
	hosts := map[string]bool{}
	for _, scheme := range []string{"http", "https"} {
		u, err := proxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: scheme}})
		if err == nil && u != nil && u.Hostname() != "" {
			hosts[strings.ToLower(u.Hostname())] = true
		}
	}
	return hosts
}

// literalLocal is a host the config spelled out as local on purpose: an IP literal or localhost.
func literalLocal(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	return net.ParseIP(strings.Trim(host, "[]")) != nil
}

// rejectLocal runs after DNS, on the resolved address; a non-public target fails before the connect.
func rejectLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("proxy: unresolvable dial target %q", address)
	}
	if textutil.IsNonPublicIP(ip, false) { // CGNAT stays open: tailnet MagicDNS names resolve there
		return fmt.Errorf(
			"proxy: %s resolves into the local network (%s); spell a local gateway as a literal address in base_url",
			address, ip)
	}
	return nil
}

// stripVia pins and strips leading /via/<host> and /d/<id> segments, either order, once each; false after a 400.
func stripVia(w http.ResponseWriter, req *http.Request) (*http.Request, bool) {
	var seen []string
	for range 3 {
		prefix := ""
		for _, p := range []string{proxy.ViaPrefix, proxy.DispatchPrefix} {
			if strings.HasPrefix(req.URL.Path, p) {
				prefix = p
			}
		}
		if prefix == "" {
			break
		}
		if slices.Contains(seen, prefix) {
			writeError(w, http.StatusBadRequest, "proxy: duplicate "+prefix+" prefix")
			return nil, false
		}
		seen = append(seen, prefix)
		seg, rest, _ := strings.Cut(req.URL.Path[len(prefix):], "/")
		ctx, err := pinSegment(req.Context(), prefix, seg)
		if err != nil {
			writeError(w, http.StatusBadRequest, "proxy: "+prefix+"<"+err.Error())
			return nil, false
		}
		out := req.WithContext(ctx)
		u := *req.URL
		u.Path, u.RawPath = "/"+rest, ""
		out.URL = &u
		req = out
	}
	return req, true
}

type dispatchKey struct{}

// pinSegment validates one stripped path segment and returns ctx carrying it: the egress host or the dispatch id.
func pinSegment(ctx context.Context, prefix, seg string) (context.Context, error) {
	if prefix == proxy.ViaPrefix {
		if err := egress.Valid(seg); err != nil {
			return nil, fmt.Errorf("host>: %w", err)
		}
		return egress.WithHost(ctx, seg), nil
	}
	if !proxy.ValidDispatchID(seg) {
		return nil, fmt.Errorf("id>: invalid dispatch id %q (digits, letters, '.', '-')", seg)
	}
	ctx = providers.WithRanIdentity(ctx, func(identity string) { proxy.WriteRanIdentity(seg, identity) })
	return context.WithValue(ctx, dispatchKey{}, seg), nil
}

// dispatchDiag is the log line's " dispatch=<id>" for a request that carried its dispatch's /d/<id> prefix.
func dispatchDiag(req *http.Request) string {
	if id, _ := req.Context().Value(dispatchKey{}).(string); id != "" {
		return " dispatch=" + id
	}
	return ""
}

// viaDiag is the log line's " via=<host>" for a request that named an egress host.
func viaDiag(req *http.Request) string {
	if h := egress.PinnedHost(req.Context()); h != "" {
		return " via=" + h
	}
	return ""
}
