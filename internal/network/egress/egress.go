// Package egress sends Databricks workspace requests through an ssh SOCKS5 tunnel on a chosen host, never direct.
package egress

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// EnvHost names the egress host for this process and its children; `claude-code --remote <host>` sets it.
const EnvHost = "HARNESS_EGRESS_HOST"

// Host is the egress host from the environment, "" when requests leave directly.
func Host() string { return os.Getenv(EnvHost) }

type hostKey struct{}

// WithHost pins the egress host on ctx: the gateway daemon carries a request's /via/<host> this way.
func WithHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, hostKey{}, host)
}

// PinnedHost is the egress host WithHost put on ctx, "" when none.
func PinnedHost(ctx context.Context) string {
	h, _ := ctx.Value(hostKey{}).(string)
	return h
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

// Seams: tests swap the workspace predicate and the ssh spawn, and shrink the start deadline.
var (
	isWorkspace = isDatabricksHost
	spawn       = spawnSSH
	startWait   = 15 * time.Second
)

// proxyFor sends workspace requests through host's tunnel, or fails them; other requests keep the env proxy.
func proxyFor(host string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if host == "" || !isWorkspace(req.URL.Hostname()) {
			return http.ProxyFromEnvironment(req)
		}
		addr, err := Ensure(req.Context(), host)
		if err != nil {
			return nil, err
		}
		return &url.URL{Scheme: "socks5h", Host: addr}, nil // net/http resolves the target name at the proxy
	}
}

// Proxy is proxyFor the host pinned on the request's context, else the environment's.
func Proxy(req *http.Request) (*url.URL, error) {
	return proxyFor(cmp.Or(PinnedHost(req.Context()), Host()))(req)
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
