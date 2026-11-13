package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/textutil"
)

// guardedClient rejects hostnames resolving locally; configured literal IPs and localhost remain allowed.
func guardedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = guardedDial
	t.Proxy = egress.Proxy // a /via/<host> request's workspace calls leave through that host's tunnel
	t.ResponseHeaderTimeout = responseHeaderTimeout
	t.MaxIdleConnsPerHost = upstreamIdleConns
	// a 3xx goes back to the caller: following it would dial a literal local IP past the guard
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

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
		for _, p := range []string{gateway.ViaPrefix, gateway.DispatchPrefix} {
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
	if prefix == gateway.ViaPrefix {
		if err := egress.Valid(seg); err != nil {
			return nil, fmt.Errorf("host>: %w", err)
		}
		return egress.WithHost(ctx, seg), nil
	}
	if !gateway.ValidDispatchID(seg) {
		return nil, fmt.Errorf("id>: invalid dispatch id %q (digits, letters, '.', '-')", seg)
	}
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
