package textutil

import (
	"strconv"
	"strings"
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
	var out []int
	for s := range strings.SplitSeq(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".") {
		part, _, _ := strings.Cut(s, "-")
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
