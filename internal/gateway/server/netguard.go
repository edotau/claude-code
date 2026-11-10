package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// guardedClient rejects hostnames resolving locally; configured literal IPs and localhost remain allowed.
func guardedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = guardedDial
	t.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: t}
}

// responseHeaderTimeout bounds the wait for upstream response headers so a wedged upstream fails over.
const responseHeaderTimeout = 120 * time.Second

var (
	plainDialer   = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guardedDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: rejectLocal}
)

// guardedDial exempts configured proxy hosts and literal local addresses before DNS resolution.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err == nil && (literalLocal(host) || proxyHosts[strings.ToLower(host)]) {
		return plainDialer.DialContext(ctx, network, addr)
	}
	return guardedDialer.DialContext(ctx, network, addr)
}

// proxyHosts holds the hostnames of HTTP(S)_PROXY as seen by http.ProxyFromEnvironment, resolved once at init.
var proxyHosts = resolveProxyHosts()

// resolveProxyHosts reads the proxy env once; net/http caches those values, so later env changes are ignored.
func resolveProxyHosts() map[string]bool {
	hosts := map[string]bool{}
	for _, scheme := range []string{"http", "https"} {
		u, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: scheme}})
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
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf(
			"proxy: %s resolves into the local network (%s); spell a local gateway as a literal address in base_url",
			address, ip)
	}
	return nil
}
