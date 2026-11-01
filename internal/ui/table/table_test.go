package table

import (
	"strings"
	"testing"
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
