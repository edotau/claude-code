package textutil

import (
	"math"
	"net"
	"net/http"
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
