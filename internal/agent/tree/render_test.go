package tree

import (
	"errors"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	tree, err := Build(fixture, Options{Days: 99999, Now: func() time.Time { return now }})
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

type renderTestWriter func([]byte) (int, error)

func (w renderTestWriter) Write(p []byte) (int, error) { return w(p) }

func TestRenderWriteErrors(t *testing.T) {
	want := errors.New("output device full")
	renders := []struct {
		name string
		run  func(io.Writer) error
	}{
		{"text", func(w io.Writer) error {
			tr := &Tree{Sessions: []Session{{Cwd: "/repo"}}}
			return RenderText(w, tr, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
		}},
	}
	failures := []struct {
		name       string
		successful int
	}{{"first-write", 0}, {"partial-output", 1}}
	for _, render := range renders {
		for _, failure := range failures {
			t.Run(render.name+"/"+failure.name, func(t *testing.T) {
				calls := 0
				w := renderTestWriter(func(p []byte) (int, error) {
					calls++
					if calls <= failure.successful {
						return len(p), nil
					}
					return len(p) / 2, want
				})
				if err := render.run(w); !errors.Is(err, want) {
					t.Fatalf("render error = %v, want %v", err, want)
				}
				if calls != failure.successful+1 {
					t.Errorf("write calls = %d, want %d; writes must stop after failure", calls, failure.successful+1)
				}
			})
		}
	}
}

func TestFormatDispatchFlagsFallback(t *testing.T) {
	fb := Dispatch{Agent: "codex", Fallback: "claude"}
	if !strings.Contains(formatDispatch(fb), "⚠ fallback") {
		t.Errorf("formatDispatch(fallback) = %q, want it to flag the fallback", formatDispatch(fb))
	}
}

func TestComputeColumnWidthsCountsRunes(t *testing.T) {
	tr := &Tree{Sessions: []Session{{
		Status:    "ééééééé",                                          // 7 runes, 14 bytes
		Subagents: []Subagent{{Type: "日本語日本語日本", Status: "完了完了完了完了"}}, // 8 runes each
	}}}
	got := computeColumnWidths(tr)
	if got.subType != 8 || got.subStatus != 8 || got.sessStatus != 7 {
		t.Errorf("widths = type %d status %d sess %d, want 8 8 7", got.subType, got.subStatus, got.sessStatus)
	}
}
