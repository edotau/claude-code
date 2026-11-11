package textutil

import (
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// CompareVersions orders dotted numeric versions, ignoring v and hyphen suffixes; false means parsing failed.
func CompareVersions(a, b string) (int, bool) {
	as, bs := versionParts(a), versionParts(b)
	if as == nil || bs == nil {
		return 0, false
	}
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x < y {
			return -1, true
		}
		if x > y {
			return 1, true
		}
	}
	return 0, true
}

func versionParts(v string) []int {
	core, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-") // drop prerelease before splitting
	var out []int
	for s := range strings.SplitSeq(core, ".") {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// BuildVersion is the module version stamped into the binary, or "devel" when none is.
func BuildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}

var majorRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(\d+)\.\d+`) })

// VersionMajor reads the major from "1.19.3", "v2.0.32" or "opencode 2.0.32"; 0 when none.
func VersionMajor(s string) int {
	m := majorRe().FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
