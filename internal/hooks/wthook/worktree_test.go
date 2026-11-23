package wthook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
)

// testHookBudget is the headroom the suite adds to every hook's budget; only expiry tests shrink it.
const testHookBudget = time.Minute

// hookBudget runs the test with hooks held to a budget of d, restoring the suite's value after.
func hookBudget(t *testing.T, name string, d time.Duration) {
	t.Helper()
	spec, ok := core.SpecOf(name)
	if !ok {
		t.Fatalf("unknown hook %q", name)
	}
	orig := core.HookMargin
	core.HookMargin = spec.Budget - d
	t.Cleanup(func() { core.HookMargin = orig })
}

// TestMain stubs the live-process scans so no wthook test reads the machine's agents or worktree users.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	core.HookMargin = -testHookBudget // every hook runs under a budget no busy machine exhausts
	code := m.Run()
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}

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
	repo := hosttest.Repo(t)

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
	repo := hosttest.Repo(t)

	first := mustCreate(t, repo, "dup")
	if second := mustCreate(t, repo, "dup"); first != second {
		t.Fatalf("paths differ: %q vs %q", first, second)
	}
}

func TestWorktreeCreateFromInsideLinkedWorktreeResolvesToMain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := hosttest.Repo(t)

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
	repo := hosttest.Repo(t)
	for _, name := range []string{"", "a/b", ".."} {
		if res := create(repo, name); res.ExitCode != 1 {
			t.Errorf("name %q: got %d, want 1; stderr %s", name, res.ExitCode, res.Stderr)
		}
	}
}

func TestWorktreeRemoveClean(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := hosttest.Repo(t)
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
	repo := hosttest.Repo(t)
	wt := mustCreate(t, repo, "led")
	ledger := paths.MemorySub("worktrees", time.Now().Format("2006-01-02")+"-led.md")
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
	repo := hosttest.Repo(t)
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
	repo := hosttest.Repo(t)
	wt := mustCreate(t, repo, "unmerged")
	hosttest.In(t, wt, "commit", "--allow-empty", "-q", "-m", "unmerged work")

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

// TestWorktreeRemoveRefusesWhenCWDIsInside pins InUseBy's cwd guard: removal refuses while the cwd is inside.
func TestWorktreeRemoveRefusesWhenCWDIsInside(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := hosttest.Repo(t)
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

// TestWorktreeBusyMode: off|false|0 is silent even with a peer; unset reports a changed peer set once.
func TestWorktreeBusyMode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	peers := []worktree.Proc{{PID: 4242, Comm: "claude"}}
	old := worktree.PeerScan
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { return peers, nil }
	defer func() { worktree.PeerScan = old }()
	busyScanTTL = 0 // every prompt scans
	t.Cleanup(func() { busyScanTTL = time.Minute })
	in := &core.Input{SessionID: "s1", CWD: cwd}

	for _, v := range []string{"off", "false", "0", "OFF"} {
		t.Setenv(worktreeModeEnv, v)
		if got := Busy(in).Context; got != "" {
			t.Fatalf("%s must be silent with a peer present, got %q", v, got)
		}
	}
	t.Setenv(worktreeModeEnv, "")
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
	old := worktree.PeerScan
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	defer func() { worktree.PeerScan = old }()
	Busy(&core.Input{SessionID: "s1", CWD: cwd})
	files, _ := filepath.Glob(filepath.Join(paths.StateDir(), "worktree-busy", "*"))
	if len(files) != 1 {
		t.Fatalf("want one state file, got %v", files)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
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
	old := worktree.PeerScan
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { <-release; return nil, nil }
	t.Cleanup(func() { close(release); worktree.PeerScan = old })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan string, 1)
	go func() {
		in := &core.Input{SessionID: "s1", CWD: cwd}
		line, _ := busyLine(in, newRun(ctx, in))
		done <- line
	}()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("busyLine = %q; want \"\" once the deadline passes", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("busyLine still blocked on the hung scan 3s after a 200ms deadline")
	}
}

// TestDivergedAtTellsSiblingCommitsFromRewrite: two new commits on one base are no rewrite; an amended pushed one is.
func TestDivergedAtTellsSiblingCommitsFromRewrite(t *testing.T) {
	for _, tc := range []struct {
		name          string
		amend         bool
		wantRewritten bool
	}{{"sibling commits", false, false}, {"amended after push", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := hosttest.Repo(t)
			commit := func(file string) {
				hosttest.WriteRel(t, repo, file, file+"\n")
				hosttest.In(t, repo, "add", file)
				hosttest.In(t, repo, "commit", "-q", "-m", file)
			}
			if tc.amend {
				commit("pushed")
				hosttest.In(t, repo, "branch", "-q", "up")
				hosttest.In(t, repo, "commit", "-q", "--amend", "-m", "pushed, reworded")
			} else {
				hosttest.In(t, repo, "branch", "-q", "up")
				hosttest.In(t, repo, "switch", "-q", "up")
				commit("remote")
				hosttest.In(t, repo, "switch", "-q", "main")
				commit("local")
			}
			hosttest.In(t, repo, "branch", "-q", "--set-upstream-to=up")
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
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "branch", "-q", "up")
	hosttest.In(t, repo, "switch", "-q", "up")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "remote")
	hosttest.In(t, repo, "switch", "-q", "main")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "local")
	hosttest.In(t, repo, "branch", "-q", "--set-upstream-to=up")
	d, err := divergedAt(context.Background(), repo)
	if err != nil || d == nil {
		t.Fatalf("divergedAt = %v, %v; want a divergence (ahead 1, behind 1)", d, err)
	}
	if d.rewritten {
		t.Errorf("rewritten = true for two empty sibling commits (ahead %d, behind %d)", d.ahead, d.behind)
	}
}

// TestAheadBehindReusesCountsWhileRefsHold: unchanged HEAD and upstream shas skip rev-list; a new commit re-counts.
func TestAheadBehindReusesCountsWhileRefsHold(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	commit := func(file string) {
		hosttest.WriteRel(t, repo, file, file+"\n")
		hosttest.In(t, repo, "add", file)
		hosttest.In(t, repo, "commit", "-q", "-m", file)
	}
	hosttest.In(t, repo, "branch", "-q", "up")
	hosttest.In(t, repo, "switch", "-q", "up")
	commit("remote")
	hosttest.In(t, repo, "switch", "-q", "main")
	commit("local")
	hosttest.In(t, repo, "branch", "-q", "--set-upstream-to=up")
	ctx := context.Background()
	key := countThenForge(t, repo)
	if a, b, _ := aheadBehind(ctx, repo); a != 7 || b != 9 {
		t.Errorf("unchanged refs = %d, %d; want the cached 7, 9", a, b)
	}
	hosttest.In(t, repo, "pack-refs", "--all")
	if k, ok := refPair(repo); !ok || k != key {
		t.Errorf("packed refs key = %q, %v; want %q", k, ok, key)
	}
	commit("local2")
	if a, b, _ := aheadBehind(ctx, repo); a != 2 || b != 1 {
		t.Errorf("after a new commit = %d, %d; want a fresh 2, 1", a, b)
	}
}

// TestRefPairKeysLinkedWorktree: a linked worktree's gitdir file and commondir still yield a key.
func TestRefPairKeysLinkedWorktree(t *testing.T) {
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "branch", "-q", "up")
	wt := filepath.Join(t.TempDir(), "side")
	hosttest.In(t, repo, "worktree", "add", "-q", "--track", "-b", "side", wt, "up")
	if k, ok := refPair(wt); !ok || strings.Count(k, "\t") != 2 {
		t.Errorf("linked worktree key = %q, %v; want a checkout, HEAD and upstream sha", k, ok)
	}
}

// TestUpstreamRefNeedsTheDefaultRefspec: a custom remote fetch refspec leaves the count uncached.
func TestUpstreamRefNeedsTheDefaultRefspec(t *testing.T) {
	dir := t.TempDir()
	branch := "[branch \"main\"]\n\tremote = origin\n\tmerge = refs/heads/main\n[remote \"origin\"]\n\tfetch = "
	for fetch, want := range map[string]bool{"+refs/heads/*:refs/remotes/origin/*": true,
		"+refs/heads/main:refs/remotes/origin/other": false} {
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(branch+fetch+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ref, ok := upstreamRef(dir, "main"); ok != want {
			t.Errorf("fetch %q: upstreamRef = %q, %v; want ok %v", fetch, ref, ok, want)
		}
	}
}

// TestUpstreamRefSkipsWorktreeConfig: extensions.worktreeConfig lets config.worktree override merge, so no key.
func TestUpstreamRefSkipsWorktreeConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := "[extensions]\n\tworktreeConfig = true\n[branch \"main\"]\n\tremote = .\n\tmerge = refs/heads/up\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if ref, ok := upstreamRef(dir, "main"); ok {
		t.Errorf("worktreeConfig: upstreamRef = %q, true; want uncached", ref)
	}
}

// TestStableAncestry: shallow, grafts, replace refs (loose or packed) and config.worktree each disable the cache.
func TestStableAncestry(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, rel := range []string{"", "shallow", "info/grafts", "refs/replace/abc", "packed-refs", "config.worktree"} {
		common := t.TempDir()
		if rel != "" {
			hosttest.WriteRel(t, common, rel, "x refs/replace/x\n")
		}
		if got := stableAncestry(common, common); got != (rel == "") {
			t.Errorf("%q: stableAncestry = %v, want %v", rel, got, rel == "")
		}
	}
}

// countThenForge counts repo (ahead 1, behind 1), then forges 7, 9 under its live key; a cache read returns those.
func countThenForge(t *testing.T, repo string) string {
	t.Helper()
	if a, b, err := aheadBehind(context.Background(), repo); a != 1 || b != 1 || err != nil {
		t.Fatalf("first count = %d, %d, %v; want 1, 1", a, b, err)
	}
	key, keyed := refPair(repo)
	if !keyed {
		t.Fatal("refPair found no key for a branch with a local upstream")
	}
	cache := filepath.Join(paths.StateDir(), "worktree-diverged", "revs-"+shortHash(key))
	if err := os.WriteFile(cache, []byte(key+"\n7\t9"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// divergedClone is a clone whose main has one local commit and one new upstream commit.
func divergedClone(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "off")
	origin := hosttest.Repo(t)
	work := filepath.Join(t.TempDir(), "work")
	hosttest.In(t, origin, "clone", "-q", origin, work)
	work = paths.Real(work)
	hosttest.WriteRel(t, origin, "up.txt", "up\n")
	hosttest.In(t, origin, "add", ".")
	hosttest.In(t, origin, "commit", "-qm", "upstream")
	hosttest.WriteRel(t, work, "mine.txt", "mine\n")
	hosttest.In(t, work, "add", ".")
	hosttest.In(t, work, "commit", "-qm", "local")
	hosttest.In(t, work, "fetch", "-q")
	orig := worktree.PeerScan
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	t.Cleanup(func() { worktree.PeerScan = orig })
	return work
}

// TestNoticesAndDivergenceReachContextOnce: a shared diverged checkout and a queued notice each surface once.
func TestNoticesAndDivergenceReachContextOnce(t *testing.T) {
	work := divergedClone(t)
	in := &core.Input{SessionID: "sB", CWD: work}
	got := Busy(in)
	if !strings.Contains(got.Context, "local main diverged from origin/main (ahead 1, behind 1)") || got.Stderr != "" {
		t.Fatalf("first prompt = %+v, want the divergence line", got)
	}
	if again := Busy(in); again.Context != "" {
		t.Fatalf("second prompt = %q, want the divergence line only once", again.Context)
	}
	if err := worktree.Append("sB", worktree.Notice{Kind: "conflict", From: "worktree merge", Text: "x.go"}); err != nil {
		t.Fatal(err)
	}
	git := func() core.Result {
		return ClaimCheck(claimInput(t, map[string]any{"session_id": "sB", "cwd": work, "tool_name": "Bash",
			"tool_input": map[string]any{"command": "git status"}}))
	}
	if r := git(); r.Context != "notice (conflict, from worktree merge): x.go" || r.Decision != core.Allow {
		t.Fatalf("git status = %+v, want the drained notice and no decision", r)
	}
	if r := git(); r.Context != "" {
		t.Fatalf("second git status = %q, want the notice gone", r.Context)
	}
}

// TestDivergenceInfraErrorWarnsAndAllows: git missing from PATH allows with a warning.
func TestDivergenceInfraErrorWarnsAndAllows(t *testing.T) {
	work := divergedClone(t)
	t.Setenv("PATH", "/nonexistent")
	got := Busy(&core.Input{SessionID: "sB", CWD: work})
	if got.Context != "" || got.Decision != core.Allow || !strings.Contains(got.Stderr, "could not check branch divergence") {
		t.Fatalf("no git = %+v, want allow + warning", got)
	}
}

// TestBusyScansPeersOnce: a diverged checkout with a peer needs the peer set twice per prompt; the scan runs once.
func TestBusyScansPeersOnce(t *testing.T) {
	work := divergedClone(t)
	t.Setenv(worktreeModeEnv, "")
	scans, stub := 0, worktree.PeerScan
	worktree.PeerScan = func(top string, nested []string) ([]worktree.Proc, error) { scans++; return stub(top, nested) }
	got := Busy(&core.Input{SessionID: "sB", CWD: work})
	if scans != 1 || !strings.Contains(got.Context, "claude pid 4242") || !strings.Contains(got.Context, "diverged") {
		t.Fatalf("scans = %d, context %q; want one scan feeding both the busy and the divergence line", scans, got.Context)
	}
}

// TestBusyReportsScanAndStateErrors: a peer-scan or state-write error warns on stderr and the prompt still proceeds.
func TestBusyReportsScanAndStateErrors(t *testing.T) {
	for _, failure := range []string{"scan peers", "write peer state"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv(worktreeModeEnv, "")
			cwd := hosttest.Repo(t)
			orig := worktree.PeerScan
			worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) {
				if failure == "scan peers" {
					return nil, errors.New("scan unavailable")
				}
				return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
			}
			t.Cleanup(func() { worktree.PeerScan = orig })
			if failure == "write peer state" { // a directory where the state file goes fails the atomic rename
				if err := os.MkdirAll(filepath.Join(paths.StateDir(), "worktree-busy", "s1"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got := Busy(&core.Input{SessionID: "s1", CWD: cwd})
			if !strings.Contains(got.Stderr, "worktree-busy: WARNING") || !strings.Contains(got.Stderr, failure) {
				t.Fatalf("missing %s warning: %+v", failure, got)
			}
			if got.Decision != core.Allow || failure == "write peer state" && !strings.Contains(got.Context, "claude pid 4242") {
				t.Fatalf("diagnostic must fail open and preserve the peer notice: %+v", got)
			}
		})
	}
}

// TestDivergedCheckDueRateLimitsPerSession: one check per session per busyScanTTL; a stale stamp is due again.
func TestDivergedCheckDueRateLimitsPerSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if !divergedCheckDue("s1") || divergedCheckDue("s1") {
		t.Fatal("first call must be due and the second inside busyScanTTL must not")
	}
	if !divergedCheckDue("s2") {
		t.Fatal("a different session has its own stamp")
	}
	stamp := filepath.Join(paths.StateDir(), "worktree-diverged", worktree.SessionSlug("s1"))
	old := time.Now().Add(-busyScanTTL - time.Second)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	if !divergedCheckDue("s1") {
		t.Fatal("a stamp older than busyScanTTL must be due again")
	}
}

// A finished audit shows as one context line on the next prompt, then never again.
func TestBusyReportsFinishedAuditOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "off")
	dir := filepath.Join(paths.StateDir(), "worktree", "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := `{"sha":"abcdef0123456789","log":"/x/abcdef01.log","started":1}`
	for name, body := range map[string]string{"abcdef01.json": rec, "abcdef01.exit": "1 42\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in := &core.Input{SessionID: "s1", CWD: t.TempDir()}
	if got := Busy(in).Context; !strings.Contains(got, "audit of abcdef01 FAILED (exit 1): /x/abcdef01.log") {
		t.Fatalf("first prompt: %q", got)
	}
	if got := Busy(in).Context; got != "" {
		t.Fatalf("second prompt must be silent, got %q", got)
	}
}
