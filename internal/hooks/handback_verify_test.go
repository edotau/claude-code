package hooks

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/plans"
)

func TestUntiedTask(t *testing.T) {
	a := plans.Task{ID: "A", Owns: []string{"w/a.go"}}
	b := plans.Task{ID: "B", Owns: []string{"x/"}}
	done := plans.Task{ID: "D", Owns: []string{"y/d.go"}, Done: true}
	for _, c := range []struct {
		name  string
		dirty []string
		want  string
	}{
		{"one owner", []string{"y/d.go", "z.go", "w/a.go"}, "A"},
		{"two owners", []string{"w/a.go", "x/b.go"}, ""},
		{"no owner", []string{"z.go"}, ""},
		{"done task skipped", []string{"y/d.go"}, ""},
	} {
		got, ok := untiedTask(plans.Plan{Tasks: []plans.Task{a, b, done}}, c.dirty)
		if got.ID != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q %v, want %q", c.name, got.ID, ok, c.want)
		}
	}
}

func TestHandbackVerifyNamesTaskAndCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, plan := planRepo(t)
	in := &core.Input{CWD: repo}
	lr := leafRead{scan: leafScan{brief: "Task: 1.2\nwire it"}}
	got := handbackVerify(in, lr, time.Now()) // an expired deadline takes the no-time branch: no gate runs
	if len(got) != 2 || !strings.HasPrefix(got[0], "verify 1.2:") ||
		!strings.HasPrefix(got[1], "commit+tick: claude-code worktree commit --task '1.2' --plan "+plan) {
		t.Errorf("task brief: got %q, want one verify 1.2 line and one commit+tick naming it", got)
	}
	other := "Task 1.3 of " + plan + " wire it"
	if got := handbackVerify(in, leafRead{scan: leafScan{brief: other}}, time.Now()); len(got) != 2 ||
		!strings.HasPrefix(got[0], "verify 1.3:") || !strings.Contains(got[1], "--plan "+plan) {
		t.Errorf("named plan: got %q, want verify 1.3 on the named plan", got)
	}
	missing := "Task 1.2 of " + filepath.Join(filepath.Dir(plan), "2026-01-01-gone.md")
	if got := handbackVerify(in, leafRead{scan: leafScan{brief: missing}}, time.Now()); got != nil {
		t.Errorf("unreadable named plan: got %q, want nothing, not another plan's task", got)
	}
	if got := handbackVerify(in, leafRead{scan: leafScan{brief: "just look around"}}, time.Now()); got != nil {
		t.Errorf("no task: got %q, want nothing", got)
	}
}
