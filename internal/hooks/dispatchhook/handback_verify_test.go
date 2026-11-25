package dispatchhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/lane"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
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

// TestCheckoutTopMatchesGit: the stat walk prints git's symlink-resolved toplevel for a subdir and a linked worktree.
func TestCheckoutTopMatchesGit(t *testing.T) {
	repo := hosttest.Repo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "side", wt)
	for _, dir := range []string{repo, sub, wt, t.TempDir()} {
		if got, want := checkoutTop(dir), memory.GitToplevel(dir); got != want {
			t.Errorf("checkoutTop(%s) = %q, want git's %q", dir, got, want)
		}
	}
}

// TestCheckoutTopResolvesSymlinks: outer/link -> other/subdir belongs to other's repo, as git sees it.
func TestCheckoutTopResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"outer/.git", "other/.git", "other/subdir"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"outer/.git/HEAD", "other/.git/HEAD"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "outer", "link")
	if err := os.Symlink(filepath.Join(root, "other", "subdir"), link); err != nil {
		t.Skip("no symlinks:", err)
	}
	if got, want := checkoutTop(link), paths.Real(filepath.Join(root, "other")); got != want {
		t.Errorf("checkoutTop(link) = %q, want %q", got, want)
	}
}

func TestVerifyLinesOneLineOnTimeout(t *testing.T) {
	cut := waves.TaskCheck{Task: "2.4", Cmd: "go vet ./x", Skip: "not finished in 13 s; the commit-time verify still runs"}
	none := waves.TaskCheck{Task: "2.4", Cmd: "-", Skip: "no checks ran"}
	failed := waves.TaskCheck{Task: "2.4", Cmd: "go test ./x", Exit: 1, Saved: "out.txt"}
	got := verifyLines([]waves.TaskCheck{failed, cut, none})
	if len(got) != 2 || !strings.Contains(got[0], "FAIL") || !strings.Contains(got[1], "not finished") {
		t.Errorf("got %q, want the failure then one not-finished line", got)
	}
}

func TestHandbackVerifyNamesTaskAndCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, plan := planRepo(t)
	in := &core.Input{CWD: repo}
	lr := leafRead{scan: leafScan{brief: "Task: 1.2\nwire it"}}
	got := handbackVerify(in, lr, time.Now()) // an expired deadline takes the no-time branch: no gate runs
	if len(got) != 2 || got[0] != "verify 1.2: lane not resolved; not run" ||
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

func TestLeafCheckoutFromMeta(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := hosttest.Repo(t)
	laneDir := filepath.Join(paths.LaneRoot(repo), "l1")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "l1", laneDir)
	tr := filepath.Join(t.TempDir(), "agent-a1.jsonl")
	in := &core.Input{AgentID: "a1", AgentTranscriptPath: tr, CWD: repo}
	if got := leafCheckout(in, repo); got != "" {
		t.Errorf("no meta, no claims: got %q, want no lane (the caller's cwd is no evidence)", got)
	}
	if err := os.WriteFile(lane.LeafMeta(tr), []byte(`{"worktreePath":"`+laneDir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := leafCheckout(in, repo); !paths.SamePath(got, laneDir) {
		t.Errorf("meta worktreePath: got %q, want %q", got, laneDir)
	}
	if got := leafCheckout(in, hosttest.Repo(t)); got != "" {
		t.Errorf("another repo's top: got %q, want no lane", got)
	}
}
