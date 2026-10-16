package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// guardedClient refuses a gateway hostname that RESOLVES into the local network (loopback, link-local,
// RFC1918, unspecified): DNS must not turn a providers.json base_url into a pivot onto the router's own
// loopback or an internal service. A literal IP or "localhost" in the config is the user's own choice
// (Ollama, a dev gateway) and dials unguarded.
func guardedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = guardedDial
	return &http.Client{Transport: t}
}

var (
	plainDialer   = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guardedDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: rejectLocal}
)

// guardedDial picks the dialer by how the host was spelled, before any DNS lookup.
func guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err == nil && literalLocal(host) {
		return plainDialer.DialContext(ctx, network, addr)
	}
	return guardedDialer.DialContext(ctx, network, addr)
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
		return fmt.Errorf("router: unresolvable dial target %q", address)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("router: %s resolves into the local network (%s); spell a local gateway as a literal address in base_url", address, ip)
	}
	return nil
}
