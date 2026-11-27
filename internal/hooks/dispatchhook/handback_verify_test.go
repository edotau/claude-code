package dispatchhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/lane"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
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
	got := handbackVerify(in, lr, time.Now()) // no owned edits: nothing is queued
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

func TestHandbackQueuesWithoutGo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, _ := planRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "go-ran")
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho ran > '"+marker+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	in := &core.Input{CWD: repo, SessionID: "main-session"}
	for range 2 {
		lines := handbackVerify(in, leafRead{scan: leafScan{brief: "Task: 1.2\nwire it"}}, time.Now().Add(time.Minute))
		if len(lines) != 2 || !strings.Contains(lines[0], "queued for main batch") || !strings.Contains(lines[1], "commit+tick:") {
			t.Fatalf("handback: %q", lines)
		}
	}
	assertHandbackPending(t, in.SessionID)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("handback ran go: %v", err)
	}
}

func assertHandbackPending(t *testing.T, session string) inspect.PendingTask {
	t.Helper()
	q, err := inspect.ReadPending(session)
	if err != nil || len(q.Tasks) != 1 {
		t.Fatalf("pending: %+v %v", q, err)
	}
	task := q.Tasks[0]
	if task.Task != "1.2" || strings.Join(task.Files, ",") != "a.go" {
		t.Fatalf("pending task: %+v", task)
	}
	return task
}

func TestHandbackRetainsCustomVerify(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, plan := planRepo(t)
	hosttest.WriteRel(t, repo, "a.go", "package a\n")
	body := "# P\n\n- [ ] 1.2 wire it verify: go test -race ./... owns: a.go\n"
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in := &core.Input{CWD: repo, SessionID: "custom"}
	lines := handbackVerify(in, leafRead{scan: leafScan{brief: "Task: 1.2"}}, time.Now())
	if len(lines) == 0 || !strings.Contains(lines[0], "queued") {
		t.Fatalf("handback: %v", lines)
	}
	task := assertHandbackPending(t, in.SessionID)
	if len(task.Checks) != 1 || task.Checks[0].Command != "go test -race ./..." {
		t.Fatalf("lost custom verify: %+v", task)
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
