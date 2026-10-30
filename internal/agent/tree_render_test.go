package agent

import (
	"math"
	"testing"
)

func TestCompactNumberBoundaries(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{math.MinInt64, "-9223372036854775808"},
		{-1000, "-1000"},
		{-1, "-1"},
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{999_999, "1000.0k"},
		{1_000_000, "1.0M"},
		{math.MaxInt64, "9223372036854.8M"},
	}
	for _, tc := range cases {
		if got := compactNumber(tc.n); got != tc.want {
			t.Errorf("compactNumber(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
