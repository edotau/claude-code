package tree

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenttree "github.com/edotau/claude-code/internal/agent/tree"
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
		{999_949, "999.9k"},
		{999_999, "1.0M"},
		{1_000_000, "1.0M"},
		{math.MaxInt64, "9223372036854.8M"},
	}
	for _, tc := range cases {
		if got := compactNumber(tc.n); got != tc.want {
			t.Errorf("compactNumber(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// (g) RenderText never produces a line over terminalWidth(), even for a subagent with a long description.
func TestRenderTextLineWidth(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)
	fixture := filepath.Join("..", "..", "agent", "tree", "testdata")
	tree, err := agenttree.Build(fixture, agenttree.Options{Days: 99999, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	RenderText(&buf, tree, now)
	max := terminalWidth()
	for i, line := range strings.Split(buf.String(), "\n") {
		if n := len([]rune(line)); n > max {
			t.Errorf("line %d is %d columns (max %d): %q", i, n, max, line)
		}
	}
	if !strings.Contains(buf.String(), "/Users/fake/demo-repo") {
		t.Error("expected the repo's cwd to head its group")
	}
}

func TestFormatDispatchFlagsFallback(t *testing.T) {
	fb := agenttree.Dispatch{Agent: "codex", Fallback: "claude"}
	if !strings.Contains(formatDispatch(fb), "⚠ fallback") {
		t.Errorf("formatDispatch(fallback) = %q, want it to flag the fallback", formatDispatch(fb))
	}
}
