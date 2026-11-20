package textutil

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxRetrySeconds = int(math.MaxInt64 / time.Second)

// HeaderRetryAfter reads retry-after-ms ahead of Retry-After; ok is false when neither parses.
func HeaderRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if ms, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After-Ms")), 10, 64); err == nil {
		return time.Duration(max(0, min(ms, int64(maxRetrySeconds)*1000))) * time.Millisecond, true
	}
	return ParseRetryAfter(h.Get("Retry-After"), now)
}

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

const (
	backoffCap    = 4 * time.Second
	retryAfterCap = 8 * time.Second // two waits at this cap keep a 3-attempt retry under ~16s
)

// RetrySleep waits d or until ctx ends; a var so tests see the wait without sleeping.
var RetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryDelay is the wait before retry number attempt (1-based): full-jitter exponential from base, never under Retry-After.
func RetryDelay(attempt int, base time.Duration, h http.Header, now time.Time) time.Duration {
	backoff := min(base<<min(max(attempt-1, 0), 20), backoffCap)
	wait := time.Duration(rand.Int64N(int64(max(backoff, 0)) + 1))
	if d, ok := HeaderRetryAfter(h, now); ok {
		wait = max(wait, min(d, retryAfterCap))
	}
	return wait
}

// WaitRetry sleeps RetryDelay on ctx; if ctx ends first it returns last (the final failed response) wrapped with
// the context error, or the bare context error when last is nil.
func WaitRetry(ctx context.Context, attempt int, base time.Duration, h http.Header, last error) error {
	err := RetrySleep(ctx, RetryDelay(attempt, base, h, time.Now()))
	if err == nil || last == nil {
		return err
	}
	return fmt.Errorf("%w: %w", last, err)
}

// StatusError describes a final HTTP status for WaitRetry's last argument.
func StatusError(status int) error { return fmt.Errorf("last response HTTP %d", status) }

// RetriableGET reports transient HTTP statuses for idempotent GET requests.
func RetriableGET(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
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
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, with the limited broadcast 255.255.255.255
	netip.MustParsePrefix("::/96"),           // IPv4-compatible IPv6 (::127.0.0.1), which Unmap does not unwrap
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, which can embed an RFC1918 IPv4
	netip.MustParsePrefix("2001::/32"),       // Teredo
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsNonPublicIP reports nil, a non-global-unicast, private or reserved address; blockCGNAT adds 100.64/10.
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
