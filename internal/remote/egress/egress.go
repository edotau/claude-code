// Package egress sends Databricks workspace requests through an ssh SOCKS5 tunnel on a chosen host, never direct.
package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/remote/tailscale"
)

// EnvHost names the egress host for this process and its children; `claude-code --remote <host>` sets it.
const EnvHost = "HARNESS_EGRESS_HOST"

type hostKey struct{}

// WithHost pins the egress host on ctx: the gateway daemon carries a request's /via/<host> this way; "" forces direct.
func WithHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, hostKey{}, host)
}

// PinnedHost is the egress host WithHost put on ctx, "" when none.
func PinnedHost(ctx context.Context) string {
	h, _ := ctx.Value(hostKey{}).(string)
	return h
}

// Alternate is ctx on the other path (direct when tunnelled, else the run's tunnel) and both paths' names.
func Alternate(ctx context.Context) (alt context.Context, from, to string, ok bool) {
	if h := HostContext(ctx); h != "" {
		return WithHost(ctx, ""), "ssh " + h, "direct", true
	}
	if h := proc.Value(ctx, EnvHost); h != "" {
		return WithHost(ctx, h), "direct", "ssh " + h, true
	}
	return ctx, "", "", false
}

// Valid accepts an ssh host that also fits one URL path segment and one file name.
func Valid(host string) error {
	if err := textutil.ValidHost(host); err != nil {
		return err
	}
	if strings.ContainsAny(host, `/\`) {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

// workspaceSuffixes are the Databricks workspace domains on AWS, Azure and GCP.
var workspaceSuffixes = []string{".cloud.databricks.com", ".azuredatabricks.net", ".gcp.databricks.com"}

// isDatabricksHost reports a Databricks workspace hostname.
func isDatabricksHost(hostname string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
	return slices.ContainsFunc(workspaceSuffixes, func(s string) bool { return strings.HasSuffix(hostname, s) })
}

// Seams: tests swap the workspace/tailnet predicates and the ssh spawn, and shrink the start deadline.
var (
	isWorkspace   = isDatabricksHost
	tailnetDirect = isTailnetDirect
	spawn         = spawnSSH
	startWait     = 15 * time.Second
)

// isTailnetDirect reports whether host is this machine or the active online Tailscale exit node.
func isTailnetDirect(ctx context.Context, host string) bool {
	if tailscale.IsLocalHost(ctx, host) {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	st, err := tailscale.CachedStatus(ctx)
	if h == "" || strings.Contains(h, "@") || err != nil {
		return false
	}
	if matchesNode(h, st.DNSName, st.IPs) {
		return true
	}
	for _, p := range st.Peers {
		if p.Online && p.ExitNode && matchesNode(h, p.DNSName, p.IPs) {
			return true
		}
	}
	return false
}

// matchesNode reports whether h (lowercased, no trailing dot) matches a tailnet node's DNSName, short name or IP.
func matchesNode(h, dns string, ips []string) bool {
	dns = strings.ToLower(strings.TrimSuffix(dns, "."))
	short, _, _ := strings.Cut(dns, ".")
	return (dns != "" && (h == dns || h == short)) ||
		slices.ContainsFunc(ips, func(ip string) bool { return strings.EqualFold(ip, h) })
}

// proxyFor sends workspace requests direct when host is on the active tailnet path, else through host's tunnel.
func proxyFor(host string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if host == "" || !isWorkspace(req.URL.Hostname()) || tailnetDirect(req.Context(), host) {
			return http.ProxyFromEnvironment(req)
		}
		addr, err := Ensure(req.Context(), host)
		if err != nil {
			return nil, err
		}
		return &url.URL{Scheme: "socks5h", Host: addr}, nil // net/http resolves the target name at the proxy
	}
}

// Proxy is proxyFor the host pinned on the request's context (even ""), else the environment's.
func Proxy(req *http.Request) (*url.URL, error) {
	return proxyFor(HostContext(req.Context()))(req)
}

var install sync.Once

// Install makes every client on http.DefaultTransport honour the egress host; without one nothing changes.
func Install() {
	install.Do(func() {
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.Proxy = Proxy
		}
	})
}

// HostContext is the host pinned on ctx (even "" = direct), else the run snapshot's (the environment's with none).
func HostContext(ctx context.Context) string {
	if host, ok := ctx.Value(hostKey{}).(string); ok {
		return host
	}
	return proc.Value(ctx, EnvHost)
}
