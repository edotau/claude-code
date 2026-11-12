package table

import (
	"strings"
	"testing" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestRenderCapsColumnsWithEllipsis(t *testing.T) {
	out := Render([]string{"NAME", "KIND"}, []int{6, 0}, [][]string{
		{"abcdefghijkl", "a-very-long-uncapped-kind"},
		{"abc", "x"},
	}, 200)
	if !strings.Contains(out, "abcde…") || strings.Contains(out, "abcdef") {
		t.Errorf("capped column not clipped to 6 runes with …:\n%s", out)
	}
	if !strings.Contains(out, "a-very-long-uncapped-kind") {
		t.Errorf("uncapped column was clipped:\n%s", out)
	}
	if !strings.Contains(out, "abc ") {
		t.Errorf("short cell altered:\n%s", out)
	}
}

func TestTruncateDelegatesAndKeepsNoCapForNonPositiveWidth(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"abcdefgh", 0, "abcdefgh"},
		{"abcdefgh", -3, "abcdefgh"},
		{"abcdefgh", 5, "abcd…"},
		{"héllo wörld", 6, "héllo…"},
		{"abc", 5, "abc"},
	}
	for _, c := range cases {
		if got := Truncate(c.s, c.width); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.s, c.width, got, c.want)
		}
	}
}

func TestDropEmptyColsShortRow(t *testing.T) {
	head, cols, rows := DropEmptyCols([]string{"A", "B", "C"}, []int{4, 0}, [][]string{{"x"}, {"y", "", "z"}})
	if len(head) != 2 || head[1] != "C" || len(cols) != 2 || cols[0] != 4 || cols[1] != 0 {
		t.Fatalf("head %v cols %v, want [A C] with widths [4 0]", head, cols)
	}
	if len(rows[0]) != 2 || rows[0][1] != "" || rows[1][1] != "z" {
		t.Errorf("rows %v, want the short row padded with an empty cell", rows)
	}
}
