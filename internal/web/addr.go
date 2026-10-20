package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/tailscale"
)

// DefaultPort is the port `claude-code web` binds by default.
const DefaultPort = 7777

// cgnat is the 100.64.0.0/10 Carrier-Grade-NAT block tailscale assigns IPv4 node addresses from.
var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isCGNAT reports whether ip is a tailnet IPv4 address (100.64.0.0/10).
func isCGNAT(ip net.IP) bool { return ip != nil && cgnat.Contains(ip) }

// defaultAddr resolves this machine's tailscale IPv4 address on DefaultPort; note is a printable
// explanation set only on the loopback fallback (tailscale down, not installed, or no IPv4 found).
func defaultAddr(ctx context.Context) (addr, note string) {
	ip, err := tailscaleIPv4(ctx)
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultPort))
	if err != nil {
		return loopback, fmt.Sprintf("web: no tailnet IPv4 address (%v); binding loopback instead", err)
	}
	return net.JoinHostPort(ip, strconv.Itoa(DefaultPort)), ""
}

// tailscaleIPv4 returns this node's tailnet (100.64.0.0/10) IPv4 address from `tailscale status`.
func tailscaleIPv4(ctx context.Context) (string, error) {
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return "", err
	}
	for _, s := range st.IPs {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil && isCGNAT(ip) {
			return s, nil
		}
	}
	return "", errors.New("tailscale has no IPv4 address for this node")
}

// validateBindAddr parses addr as host:port and refuses anything but a loopback or tailnet
// (100.64.0.0/10) literal IP — never 0.0.0.0, ::, an empty host, or a hostname.
func validateBindAddr(addr string) (net.IP, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web: --addr %q: %w", addr, err)
	}
	if host == "" {
		return nil, fmt.Errorf("web: --addr %q: binding every interface is refused", addr)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("web: --addr %q: bad port: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("web: --addr %q: host must be a literal IP, not a name", addr)
	}
	if !ip.IsLoopback() && !isCGNAT(ip) {
		return nil, fmt.Errorf("web: --addr %q: refused — only a loopback or tailnet 100.64.0.0/10 address may be bound", addr)
	}
	return ip, nil
}

// tailnetNames returns this node's MagicDNS name, full and short (before the first "."), lowercased
// and with any trailing dot trimmed; empty when tailscale status can't be read.
func tailnetNames(ctx context.Context) []string {
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return nil
	}
	name := strings.ToLower(strings.TrimSuffix(st.DNSName, "."))
	if name == "" {
		return nil
	}
	names := []string{name}
	if short, _, ok := strings.Cut(name, "."); ok {
		names = append(names, short)
	}
	return names
}
