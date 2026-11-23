package tree

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// dashNodes is s1 > a > d1, then s2 and a root dispatch r; roots start open, a starts closed.
func dashNodes() []Node {
	d1 := Node{Kind: KindDispatch, ID: "d1", Depth: 2}
	a := Node{Kind: KindSubagent, ID: "a", Depth: 1, Children: []Node{d1}}
	return []Node{{Kind: KindSession, ID: "s1", Children: []Node{a}}, {Kind: KindSession, ID: "s2"},
		{Kind: KindDispatch, ID: "r"}}
}

func press(s dashState, nodes []Node, keys ...key) dashState {
	for _, k := range keys {
		s = onKey(s, k, nodes)
	}
	return s
}

func TestDashboardMoveExpandCollapse(t *testing.T) {
	nodes := dashNodes()
	s := clamp(dashState{height: 24}, visible(nodes, nil))
	for _, c := range []struct {
		k        key
		sel      string
		visCount int
	}{
		{keyUp, "session:s1", 4}, {keyDown, "subagent:a", 4}, {keyRight, "subagent:a", 5},
		{keyRight, "dispatch:d1", 5}, {keyLeft, "subagent:a", 5}, {keyLeft, "subagent:a", 4},
		{keyLeft, "session:s1", 4}, {keyLeft, "session:s1", 3}, {keyDown, "session:s2", 3},
	} {
		s = press(s, nodes, c.k)
		if n := len(visible(nodes, s.open)); s.selID != c.sel || n != c.visCount {
			t.Fatalf("after key %d: sel %q with %d rows, want %q with %d", c.k, s.selID, n, c.sel, c.visCount)
		}
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

func rootNodes(n int) []Node {
	nodes := make([]Node, n)
	for i := range nodes {
		nodes[i] = Node{Kind: KindDispatch, ID: strconv.Itoa(i)}
	}
	return nodes
}

func TestDashboardScrollClamp(t *testing.T) {
	nodes := rootNodes(10)
	s := clamp(dashState{height: 5}, visible(nodes, nil)) // 3 rows above the footer
	if s = press(s, nodes, keyDown, keyDown, keyDown, keyDown, keyDown); s.sel != 5 || s.top != 3 {
		t.Fatalf("down x5: sel %d top %d, want 5 and 3", s.sel, s.top)
	}
	if s = press(s, nodes, keyUp, keyUp, keyUp, keyUp, keyUp, keyUp); s.sel != 0 || s.top != 0 {
		t.Fatalf("up past the top: sel %d top %d, want 0 and 0", s.sel, s.top)
	}
}

func TestDashboardResizeClamp(t *testing.T) {
	nodes := rootNodes(10)
	s := clamp(dashState{height: 4, selID: "dispatch:9"}, visible(nodes, nil))
	if s.sel != 9 || s.top != 8 {
		t.Fatalf("sel %d top %d, want 9 and 8", s.sel, s.top)
	}
	if s = onResize(s, 80, 30); s.top != 0 {
		t.Errorf("taller terminal: top %d, want 0", s.top)
	}
	if s = onResize(s, 80, 3); s.top != 9 {
		t.Errorf("one-row terminal: top %d, want 9 (the selection)", s.top)
	}
}

// The real row renderer keeps every dashboard line inside the terminal width, the selected row included.
func TestDashboardRowsFitWidth(t *testing.T) {
	long := strings.Repeat("word ", 40)
	sess := &Session{ID: "0123456789abcdef", Name: "a-rather-long-session-name", Status: statusActive,
		Branch: "feature/very-long-branch-name", Title: long}
	sub := &Subagent{ID: "agent1", Type: "general-purpose", Status: statusRunning,
		Description: "databricks-claude-sonnet-4-6: " + long}
	d := &Dispatch{ID: "job1", Agent: "codex", Model: "databricks-gpt-5-5-codex-extra-long-model-id",
		Provider: "databricks", Status: statusRunning, Duration: "15m30s"}
	nodes := []Node{{Kind: KindSession, ID: sess.ID, Session: sess, Children: []Node{{Kind: KindSubagent, ID: sub.ID,
		Depth: 1, Subagent: sub, Children: []Node{{Kind: KindDispatch, ID: d.ID, Depth: 2, Dispatch: d}}}}}}
	open := map[string]bool{"subagent:agent1": true}
	for _, width := range []int{80, 40} {
		s := clamp(dashState{open: open, width: width, height: 24, selID: "subagent:agent1"}, visible(nodes, open))
		lines := dashLines(s, nodes, time.Now(), "")
		if len(lines) != 5 {
			t.Fatalf("width %d: %d lines, want 3 rows and the footer:\n%s", width, len(lines), strings.Join(lines, "\n"))
		}
		for _, line := range lines {
			if n := textutil.DisplayWidth(ansi.Strip(line)); n > width {
				t.Errorf("width %d: line is %d columns: %q", width, n, line)
			}
		}
	}
}
