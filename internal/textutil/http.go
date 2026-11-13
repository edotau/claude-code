package textutil

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxRetrySeconds is the largest second count that still fits a time.Duration.
const maxRetrySeconds = int(math.MaxInt64 / time.Second)

// ParseRetryAfter parses seconds or an HTTP-date; callers decide how to handle nonpositive durations.
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if seconds, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(-maxRetrySeconds, min(seconds, maxRetrySeconds))) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return at.Sub(now), true
	}
	return 0, false
}

// IsLoopbackHost accepts localhost case-insensitively and loopback IPs with optional brackets.
func IsLoopbackHost(h string) bool {
	h = strings.Trim(h, "[]")
	ip := net.ParseIP(h)
	return strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())
}

// IsPlainHTTPOffLoopback reports a plain http URL whose host is not loopback, where credentials must not go.
func IsPlainHTTPOffLoopback(u *url.URL) bool {
	return u.Scheme == "http" && !IsLoopbackHost(u.Hostname())
}

// nonPublicRanges are what netip's predicates miss; CGNAT is separate since tailnet names resolve there.
var nonPublicRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),   // "this network"
	netip.MustParsePrefix("240.0.0.0/4"), // reserved, with the limited broadcast 255.255.255.255
	netip.MustParsePrefix("::/96"),       // IPv4-compatible IPv6 (::127.0.0.1), which Unmap does not unwrap
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsNonPublicIP reports nil or an address that is not global unicast, private, or in a reserved range; blockCGNAT adds 100.64/10.
func IsNonPublicIP(ip net.IP, blockCGNAT bool) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap() // ::ffff:100.64.0.1 matches 100.64.0.0/10
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || (blockCGNAT && cgnat.Contains(addr)) {
		return true
	}
	return slices.ContainsFunc(nonPublicRanges, func(p netip.Prefix) bool { return p.Contains(addr) })
}
