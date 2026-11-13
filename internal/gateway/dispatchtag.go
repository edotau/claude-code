package gateway

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// DispatchPrefix opens a router path that tags the request with its dispatch id: /d/<id>/…, composable with /via/.
const DispatchPrefix = "/d/"

// DispatchBase is base plus /d/<id> for dispatch id; an id the proxy would refuse is left off.
func DispatchBase(base, id string) string {
	if !ValidDispatchID(id) {
		return base
	}
	return base + DispatchPrefix + id
}

// TaggedBase is DispatchBase only when the daemon at base advertises dispatch tags; an older daemon would 404 /d/.
func TaggedBase(ctx context.Context, base, id string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return base
	}
	if st, ok := Holder(ctx, port); !ok || !st.DispatchTag {
		return base
	}
	return DispatchBase(base, id)
}

// UntaggedBase is raw without a leading /d/<id> path segment pair, so a tagged base compares as its /p/<profile> route.
func UntaggedBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	rest, ok := strings.CutPrefix(u.Path, DispatchPrefix)
	if !ok {
		return raw
	}
	id, tail, _ := strings.Cut(rest, "/")
	if !ValidDispatchID(id) {
		return raw
	}
	u.Path, u.RawPath = "/"+tail, ""
	return u.String()
}

const maxDispatchIDLen = 128

// ValidDispatchID reports whether id is a dispatch id the proxy may log: 1-128 of digits, letters, '.' and '-'.
func ValidDispatchID(id string) bool {
	if id == "" || len(id) > maxDispatchIDLen {
		return false
	}
	for _, r := range id {
		if !dispatchIDRune(r) {
			return false
		}
	}
	return true
}

func dispatchIDRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-'
}
