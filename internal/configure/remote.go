package configure

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
)

// cgnatRemote is the 100.64.0.0/10 block tailscale assigns IPv4 node addresses from.
var cgnatRemote = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// runRemote reports tailscale connectivity, dispatch hosts, and whether `claude-code web` has a
// tailnet address to bind — read-only, never fails --check.
func runRemote(d *dialog) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	st, err := tailscale.Status(ctx)
	switch {
	case err == nil:
		d.printf("tailscale   running (%s)\n", strings.TrimSuffix(st.DNSName, "."))
	default:
		d.printf("tailscale   not connected → claude-code tailscale connect\n")
	}
	switch {
	case hasAdminCredential():
		d.printf("admin api   credential present\n")
	default:
		d.printf("admin api   credential absent → claude-code secrets set TAILSCALE_OAUTH_CLIENT_ID|_SECRET\n")
	}
	machines, _ := tailscale.Machines(ctx)
	var online []string
	for _, m := range machines {
		if m.Online && m.Name != "" {
			online = append(online, m.Name)
		}
	}
	if len(online) > 0 {
		d.printf("dispatch    %s\n", strings.Join(online, ", "))
	} else {
		d.printf("dispatch    no online tailnet machines found\n")
	}
	if ip := tailnetIPv4(st); ip != "" {
		d.printf("web         %s available for claude-code web\n", ip)
	} else {
		d.printf("web         no tailnet IPv4 address; claude-code web binds loopback only\n")
	}
	return true
}

// hasAdminCredential reports, by name only, whether a tailscale admin API credential is set.
func hasAdminCredential() bool {
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_ID"); ok {
		return true
	}
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok {
		return true
	}
	_, _, ok := secrets.Get("TAILSCALE_API_KEY")
	return ok
}

// tailnetIPv4 picks this node's tailnet (100.64.0.0/10) IPv4 address out of st.IPs, if any.
func tailnetIPv4(st tailscale.Local) string {
	for _, s := range st.IPs {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil && cgnatRemote.Contains(ip) {
			return s
		}
	}
	return ""
}
