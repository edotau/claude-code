package wthook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// createPayload is the real WorktreeCreate shape Claude Code sends: only cwd + name.
func createPayload(cwd, name string) *core.Input {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": cwd, "hook_event_name": "WorktreeCreate", "name": name,
	})
	return core.ParseInput(bytes.NewReader(b))
}

// create runs the WorktreeCreate runner and returns its Result.
func create(cwd, name string) core.Result { return Create(createPayload(cwd, name)) }

// mustCreate creates a worktree and returns its path, failing the test on a non-zero exit.
func mustCreate(t *testing.T, cwd, name string) string {
	t.Helper()
	res := create(cwd, name)
	if res.ExitCode != core.ExitProceed {
		t.Fatalf("create %q: exit %d, stderr %s", name, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// remove runs the WorktreeRemove helper and returns its exit code and stderr.
func remove(cwd, worktreePath string) (int, string) {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": cwd, "hook_event_name": "WorktreeRemove", "worktree_path": worktreePath,
	})
	var errb bytes.Buffer
	code := Remove(core.ParseInput(bytes.NewReader(b)), &errb)
	return code, errb.String()
}

func TestWorktreeCreateFromRealPayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gittest.Repo(t)

	got := mustCreate(t, repo, "live-probe")
	want := filepath.Join(canonHome(t, home), "worktrees", filepath.Base(repo), "live-probe")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	branchOut, err := exec.Command("git", "-C", got, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(branchOut)) != "worktree-live-probe" {
		t.Fatalf("branch = %q, err %v", branchOut, err)
	}
}

func TestWorktreeCreateRepeatReturnsSamePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)

	first := mustCreate(t, repo, "dup")
	if second := mustCreate(t, repo, "dup"); first != second {
		t.Fatalf("paths differ: %q vs %q", first, second)
	}
}

func TestWorktreeCreateFromInsideLinkedWorktreeResolvesToMain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gittest.Repo(t)

	linked := mustCreate(t, repo, "first")
	// A second worktree requested with cwd set to the first linked checkout must still land under the main repo.
	want := filepath.Join(canonHome(t, home), "worktrees", filepath.Base(repo), "second")
	if got := mustCreate(t, linked, "second"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestWorktreeCreateNonRepoCWD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if res := create(t.TempDir(), "x"); res.ExitCode != 1 {
		t.Fatalf("got %d, want 1; stderr %s", res.ExitCode, res.Stderr)
	}
}

func TestWorktreeCreateBadName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	for _, name := range []string{"", "a/b", ".."} {
		if res := create(repo, name); res.ExitCode != 1 {
			t.Errorf("name %q: got %d, want 1; stderr %s", name, res.ExitCode, res.Stderr)
		}
	}
}

func TestWorktreeRemoveClean(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	wt := mustCreate(t, repo, "clean")

	if code, stderr := remove(wt, wt); code != core.ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone, stat err = %v", err)
	}
	out2, err := exec.Command("git", "-C", repo, "branch", "--list", "worktree-clean").Output()
	if err != nil || strings.TrimSpace(string(out2)) != "" {
		t.Fatalf("merged branch should be deleted, branch --list = %q, err %v", out2, err)
	}
}

func TestWorktreeRemoveClosesLedger(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	wt := mustCreate(t, repo, "led")
	ledger := filepath.Join(memory.Dir(repo), "worktrees", "led.md")
	if data, err := os.ReadFile(ledger); err != nil || !strings.Contains(string(data), "- Status: open") {
		t.Fatalf("ledger not opened by create: %v\n%s", err, data)
	}

	if code, stderr := remove(wt, wt); code != core.ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, stderr)
	}
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Status: closed", "- Via: remove", "Branch: deleted"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("ledger missing %q:\n%s", want, data)
		}
	}
}

func TestWorktreeRemoveDirty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	wt := mustCreate(t, repo, "dirty")
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, stderr := remove(wt, wt); code != 1 {
		t.Fatalf("remove dirty: got %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dirty worktree dir should stay, stat err = %v", err)
	}
}

func TestWorktreeRemoveKeepsUnmergedBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	wt := mustCreate(t, repo, "unmerged")
	gittest.In(t, wt, "commit", "--allow-empty", "-q", "-m", "unmerged work")

	if code, stderr := remove(wt, wt); code != core.ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone, stat err = %v", err)
	}
	branchOut, err := exec.Command("git", "-C", repo, "branch", "--list", "worktree-unmerged").Output()
	if err != nil || strings.TrimSpace(string(branchOut)) == "" {
		t.Fatalf("unmerged branch should be kept, branch --list = %q, err %v", branchOut, err)
	}
}

// TestWorktreeRemoveRefusesWhenCWDIsInside is the hook-path regression test for InUseBy's cwd guard:
// WorktreeRemove must refuse (and leave the worktree in place) when the calling process's own cwd is
// still inside the worktree being removed, the one in-use case this package can trigger without
// stubbing worktree's unexported process-discovery seam.
func TestWorktreeRemoveRefusesWhenCWDIsInside(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gittest.Repo(t)
	wt := mustCreate(t, repo, "cwdbusy")
	t.Chdir(wt)

	code, stderr := remove(wt, wt)
	if code != 1 {
		t.Fatalf("remove: got %d, want 1 (refused); stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "cd out") {
		t.Errorf("stderr = %q, want it to mention cd-ing out", stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree should still exist after a refused remove: %v", err)
	}
}

// canonHome resolves a temp HOME the way WorktreeRoot does (macOS tmp dirs live behind a /var symlink).
func canonHome(t *testing.T, home string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestWorktreeModeAndBusy: off|false|0 is silent even with a peer; unset asks at start and reports a changed peer set once.
func TestWorktreeModeAndBusy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	peers := []worktree.Proc{{PID: 4242, Comm: "claude"}}
	old := PeerAgents
	PeerAgents = func(string, []string) ([]worktree.Proc, error) { return peers, nil }
	defer func() { PeerAgents = old }()
	in := &core.Input{SessionID: "s1", CWD: cwd}

	for _, v := range []string{"off", "false", "0", "OFF"} {
		t.Setenv(worktreeModeEnv, v)
		if got := Mode(in).Context + Busy(in).Context; got != "" {
			t.Fatalf("%s must be silent with a peer present, got %q", v, got)
		}
	}
	t.Setenv(worktreeModeEnv, "")
	if got := Mode(in).Context; got != worktreeAskLine {
		t.Fatalf("unset start: %q", got)
	}
	if got := Busy(in).Context; !strings.Contains(got, "claude pid 4242") {
		t.Fatalf("unset busy: %q", got)
	}
	if got := Busy(in).Context; got != "" {
		t.Fatalf("same peers must dedupe, got %q", got)
	}
	peers = []worktree.Proc{{PID: 4242, Comm: "claude"}, {PID: 77, Comm: "codex"}}
	if got := Busy(in).Context; got == "" {
		t.Fatal("changed peer set must re-report")
	}
	peers = nil
	if got := Busy(in).Context; got != "" {
		t.Fatalf("no peers: %q", got)
	}
}

// TestWorktreeBusyStateFileIs0600: the busy-peer state file is private to the owner.
func TestWorktreeBusyStateFileIs0600(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "")
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	old := PeerAgents
	PeerAgents = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	defer func() { PeerAgents = old }()
	Busy(&core.Input{SessionID: "s1", CWD: cwd})
	files, _ := filepath.Glob(filepath.Join(paths.StateDir(), "worktree-busy", "*"))
	if len(files) != 1 {
		t.Fatalf("want one state file, got %v", files)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
}

// TestCheckoutOfPassesOnlyNestedWorktrees: a nested worktree never skips the main tree, which holds all its peers.
func TestCheckoutOfPassesOnlyNestedWorktrees(t *testing.T) {
	main := core.CanonPath(gittest.Repo(t))
	nested := filepath.Join(main, ".wt", "x")
	gittest.In(t, main, "worktree", "add", "-q", "-b", "x", nested)
	ctx, cancel := core.HookContext("worktree-busy")
	defer cancel()
	if top, _, skip := CheckoutOf(ctx, nested); top != nested || len(skip) != 0 {
		t.Fatalf("from the nested worktree: top %q, skip %v; want top %q and no skip dirs", top, skip, nested)
	}
	if top, _, skip := CheckoutOf(ctx, main); top != main || len(skip) != 1 || skip[0] != nested {
		t.Fatalf("from the main tree: top %q, skip %v; want top %q and skip [%s]", top, skip, main, nested)
	}
}

// TestBusyLineAbandonsHungScan: a process scan that never returns must not outlive the hook deadline.
func TestBusyLineAbandonsHungScan(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	old := PeerAgents
	PeerAgents = func(string, []string) ([]worktree.Proc, error) { <-release; return nil, nil }
	t.Cleanup(func() { close(release); PeerAgents = old })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- busyLine(ctx, &core.Input{SessionID: "s1", CWD: cwd}) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("busyLine = %q; want \"\" once the deadline passes", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("busyLine still blocked on the hung scan 3s after a 200ms deadline")
	}
}

// TestDivergedAtTellsSiblingCommitsFromRewrite: two new commits on one base are not a rewrite; an amended pushed one is.
func TestDivergedAtTellsSiblingCommitsFromRewrite(t *testing.T) {
	for _, tc := range []struct {
		name          string
		amend         bool
		wantRewritten bool
	}{{"sibling commits", false, false}, {"amended after push", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.Repo(t)
			commit := func(file string) {
				gittest.WriteRel(t, repo, file, file+"\n")
				gittest.In(t, repo, "add", file)
				gittest.In(t, repo, "commit", "-q", "-m", file)
			}
			if tc.amend {
				commit("pushed")
				gittest.In(t, repo, "branch", "-q", "up")
				gittest.In(t, repo, "commit", "-q", "--amend", "-m", "pushed, reworded")
			} else {
				gittest.In(t, repo, "branch", "-q", "up")
				gittest.In(t, repo, "switch", "-q", "up")
				commit("remote")
				gittest.In(t, repo, "switch", "-q", "main")
				commit("local")
			}
			gittest.In(t, repo, "branch", "-q", "--set-upstream-to=up")
			d, err := divergedAt(context.Background(), repo)
			if err != nil || d == nil {
				t.Fatalf("divergedAt = %v, %v; want a divergence (ahead 1, behind 1)", d, err)
			}
			if d.rewritten != tc.wantRewritten {
				t.Errorf("rewritten = %v, want %v (ahead %d, behind %d)", d.rewritten, tc.wantRewritten, d.ahead, d.behind)
			}
		})
	}
}

// TestDivergedAtEmptyCommitsAreNotARewrite: empty commits on both sides share one patch-id but replace nothing.
func TestDivergedAtEmptyCommitsAreNotARewrite(t *testing.T) {
	repo := gittest.Repo(t)
	gittest.In(t, repo, "branch", "-q", "up")
	gittest.In(t, repo, "switch", "-q", "up")
	gittest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "remote")
	gittest.In(t, repo, "switch", "-q", "main")
	gittest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "local")
	gittest.In(t, repo, "branch", "-q", "--set-upstream-to=up")
	d, err := divergedAt(context.Background(), repo)
	if err != nil || d == nil {
		t.Fatalf("divergedAt = %v, %v; want a divergence (ahead 1, behind 1)", d, err)
	}
	if d.rewritten {
		t.Errorf("rewritten = true for two empty sibling commits (ahead %d, behind %d)", d.ahead, d.behind)
	}
}
