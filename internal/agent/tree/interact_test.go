package tree

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

func TestDecodeKeys(t *testing.T) {
	got := decodeKeys([]byte("\x1b[A\x1bOBj\rq\x1bx\x1b[3~"))
	want := []key{keyUp, keyDown, keyDown, keyEnter, keyQuit, keyEsc}
	if !slices.Equal(got, want) {
		t.Errorf("decodeKeys = %v, want %v", got, want)
	}
}

func TestPickClampsAndRecovers(t *testing.T) {
	ids := []string{"a", "b", "c"}
	for _, c := range []struct {
		sel  string
		step int
		want string
	}{{"", 0, "a"}, {"a", -1, "a"}, {"b", 1, "c"}, {"c", 1, "c"}, {"gone", 1, "a"}} {
		if got := pick(ids, c.sel, c.step); got != c.want {
			t.Errorf("pick(%q, %d) = %q, want %q", c.sel, c.step, got, c.want)
		}
	}
	if got := pick(nil, "a", 1); got != "" {
		t.Errorf("pick(no rows) = %q", got)
	}
}

// The detail view fits the terminal: the task wraps to three lines, output keeps the newest lines, nothing wraps.
func TestDetailLinesFit(t *testing.T) {
	out := &followBuf{}
	for i := range 50 {
		_, _ = out.Write([]byte("\x1b[31mline\t" + strings.Repeat("x", i*3) + "\x1b[0m\n"))
	}
	_, _ = out.Write([]byte("partial"))
	head := []string{"agent  codex", "task   " + strings.Repeat("word ", 60)}
	lines := detailLines(head, out.tail(), 80, 24)
	if len(lines) != 24 || lines[len(lines)-1] != jobFooter || lines[len(lines)-3] != "partial" {
		t.Fatalf("%d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[3], "word") || !strings.HasSuffix(lines[3], "…") {
		t.Errorf("task line 3 = %q, want the wrapped, clipped task", lines[3])
	}
	for _, line := range lines {
		if n := textutil.DisplayWidth(line); n > 80 || strings.ContainsAny(line, "\x1b\t") {
			t.Errorf("line is %d columns or keeps an escape: %q", n, line)
		}
	}
}
