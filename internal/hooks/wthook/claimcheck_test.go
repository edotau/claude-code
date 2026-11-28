package wthook

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// claimFixture is a repo with a.txt and b.txt committed, a config dir, and liveOwner as the only live pid.
func claimFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := paths.Real(hosttest.Repo(t))
	hosttest.WriteRel(t, repo, "a.txt", "a\n")
	hosttest.WriteRel(t, repo, "b.txt", "b\n")
	hosttest.In(t, repo, "add", ".")
	hosttest.In(t, repo, "commit", "-qm", "files")
	orig := claimAlive
	claimAlive = func(pid int) bool { return pid == liveOwner }
	t.Cleanup(func() { claimAlive = orig })
	return repo
}

// claimEdit records an Edit of rel by session (owner pid) and leaves rel dirty.
func claimEdit(t *testing.T, repo string, owner int, session, rel string) {
	t.Helper()
	useClaimOwner(t, owner)
	hosttest.WriteRel(t, repo, rel, "edited by "+session+"\n")
	Claim(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": rel}}))
	useClaimOwner(t, checkerOwner) // the checks that follow run as another process
}

// resetClaimCase forgets every recorded claim, so cases can share one fixture repo.
func resetClaimCase(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(worktree.ClaimsDir()); err != nil {
		t.Fatal(err)
	}
}

func claimCheckOn(t *testing.T, repo, session, tool string, input map[string]any) core.Result {
	t.Helper()
	useClaimOwner(t, checkerOwner) // the check runs as a process other than the claimants
	return ClaimCheck(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": tool,
		"tool_input": input}))
}

// TestClaimCheckNeverRefusesStaging: staging or committing a peer's claimed file is allowed for main, leaf and verb.
func TestClaimCheckNeverRefusesStaging(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	t.Setenv("CLAUDE_CONFIG_DIR", repo) // a leaf under the config tree was the one blocked before
	for _, cmd := range []string{"git add -- a.txt", "git add -A", "git commit -am x", "git commit -m x -- a.txt",
		"bash -c 'git add -A'", "eval eval eval eval eval git add -A", "claude-code worktree commit -m x a.txt",
		"claude-code worktree commit -m x"} {
		for _, agent := range []string{"", "leafone1"} {
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": agent, "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}))
			if r.Decision != core.Allow {
				t.Errorf("%q (agent %q): %v %q; want allow", cmd, agent, r.Decision, r.Reason)
			}
		}
	}
	if err := os.Remove(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "add", "--", "a.txt")
	useClaimOwner(t, liveOwner)
	Claim(claimInput(t, map[string]any{"session_id": "deleter", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add -- a.txt"}}))
	useClaimOwner(t, checkerOwner)
	claims, err := liveClaims(&core.Input{SessionID: "A"})
	if err != nil || len(claims[filepath.Join(repo, "a.txt")]) != 1 {
		t.Fatalf("staged deletion lost its claim: %v, err = %v", claims, err)
	}
}

// TestClaimCheck: a live peer's dirty claim advises once; a dead owner's claim and file go.
func TestClaimCheck(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	claimEdit(t, repo, deadOwner, "D", "b.txt")
	edit := map[string]any{"file_path": filepath.Join(repo, "a.txt")}
	if r := claimCheckOn(t, repo, "B", "Edit", edit); !strings.Contains(r.Context, "a.txt has uncommitted edits by") {
		t.Fatalf("first edit of A's file: context %q; want the advisory", r.Context)
	}
	if r := claimCheckOn(t, repo, "B", "Edit", edit); r.Context != "" {
		t.Fatalf("repeat edit must be silent, got %q", r.Context)
	}
	bEdit := map[string]any{"file_path": filepath.Join(repo, "b.txt")}
	if r := claimCheckOn(t, repo, "B", "Edit", bEdit); r.Context != "" {
		t.Fatalf("edit of b.txt: context %q; want none (b.txt's owner is dead)", r.Context)
	}
	if _, err := os.Stat(claimsFile(deadOwner, "D")); !os.IsNotExist(err) {
		t.Fatalf("dead owner's claims file must be removed, stat err = %v", err)
	}
}

// TestClaimCheckOwnPidNewSlug: after /clear or a resume the same pid files under a new slug; that is no peer.
func TestClaimCheckOwnPidNewSlug(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkPid int
		wantNote bool
	}{{"same pid, new session", liveOwner, false}, {"other pid, other session", checkerOwner, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "OLD", "a.txt")
			useClaimOwner(t, tc.checkPid)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "NEW", "cwd": repo, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": filepath.Join(repo, "a.txt")}}))
			if noted := r.Context != ""; noted != tc.wantNote {
				t.Fatalf("edit as pid %d: context %q; want note = %v", tc.checkPid, r.Context, tc.wantNote)
			}
		})
	}
}

// TestClaimCheckDropsLandedClaim: a claim older than the file's last commit has landed, so an edit notes nothing.
func TestClaimCheckDropsLandedClaim(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.In(t, repo, "add", "a.txt")
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Hour).Format(time.RFC3339))
	hosttest.In(t, repo, "commit", "-qm", "A lands a.txt")
	hosttest.WriteRel(t, repo, "a.txt", "B's own edit\n")
	edit := map[string]any{"file_path": filepath.Join(repo, "a.txt")}
	if r := claimCheckOn(t, repo, "B", "Edit", edit); r.Context != "" {
		t.Fatalf("landed claim: context %q; want none", r.Context)
	}
}

// TestStageOperandsCommitUntrackedIsNotAll: on commit -u is --untracked-files, so only -a/--all/-am stage all tracked.
func TestStageOperandsCommitUntrackedIsNotAll(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		all  bool
	}{
		{"commit", []string{"-u", "-m", "x"}, false},
		{"commit", []string{"--update", "-m", "x"}, false},
		{"commit", []string{"-A", "-m", "x"}, false},
		{"commit", []string{"-a", "-m", "x"}, true},
		{"commit", []string{"--all", "-m", "x"}, true},
		{"commit", []string{"-am", "x"}, true},
		{"commit", []string{"-uall", "-m", "x"}, false},
		{"commit", []string{"-unormal", "-m", "x"}, false},
		{"commit", []string{"-uno", "-m", "x"}, false},
		{"commit", []string{"-sa", "-m", "x"}, true},
		{"add", []string{"-u"}, true},
	}
	for _, c := range cases {
		if all := splitStage(c.verb, c.args).stagesAll(c.verb); all != c.all {
			t.Errorf("splitStage(%s, %v) all=%v, want %v", c.verb, c.args, all, c.all)
		}
	}
}

// TestFirstSightingPersistsAcrossKeys: a key shows once, and a second distinct key never drops the first.
func TestFirstSightingPersistsAcrossKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i, step := range []struct {
		key  string
		want bool
	}{{"k1", true}, {"k1", false}, {"k2", true}, {"k1", false}, {"k2", false}} {
		if got := firstSighting("sess", step.key); got != step.want {
			t.Fatalf("step %d firstSighting(%q)=%v, want %v", i, step.key, got, step.want)
		}
	}
}

// TestFirstSightingLeavesNoLockFile: the seen file locks itself, so no .lock sibling is left behind.
func TestFirstSightingLeavesNoLockFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	firstSighting("sess", "k1")
	dir := filepath.Dir(seenFile("sess"))
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || strings.HasSuffix(ents[0].Name(), ".lock") {
		t.Fatalf("claims-seen dir = %v (err %v); want only the seen file", ents, err)
	}
}

// TestFirstSightingFailsOpenOnHeldLock: a held lock costs at most seenWait, then the advisory still shows.
func TestFirstSightingFailsOpenOnHeldLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := seenFile("sess")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if !paths.TryLock(held) {
		t.Skip("file locking unsupported")
	}
	const wait = 50 * time.Millisecond
	t.Cleanup(core.SwapSeenWait(wait))
	start := time.Now()
	if !firstSighting("sess", "k1") {
		t.Fatal("held lock: want fail-open true")
	}
	if took := time.Since(start); took < wait {
		t.Fatalf("held lock: returned after %v, before the %v wait", took, wait)
	}
}

// TestClaimCheckExpiredBudgetFailsOpen: a check past its deadline allows with a warning instead of a verdict.
func TestClaimCheckExpiredBudgetFailsOpen(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hookBudget(t, "worktree-claim-check", time.Nanosecond)
	r := claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": filepath.Join(repo, "a.txt")})
	if r.Decision != core.Allow || !strings.Contains(r.Stderr, "could not check file claims") {
		t.Fatalf("expired budget: %v %q; want allow with the warning", r.Decision, r.Stderr)
	}
}

// TestStageOperandsAttachedValueTakesNoNextWord: git reads `-mc x` as message "c" with pathspec x.
func TestStageOperandsAttachedValueTakesNoNextWord(t *testing.T) {
	cases := []struct {
		args []string
		ops  []string
	}{
		{[]string{"-mc", "x"}, []string{"x"}},
		{[]string{"-um", "x"}, []string{"x"}},
		{[]string{"-aum", "x"}, []string{"x"}},
		{[]string{"-am", "msg", "y"}, []string{"y"}},
		{[]string{"-m", "msg", "y"}, []string{"y"}},
		{[]string{"--trailer", "Reviewed-by: x"}, nil},
		{[]string{"--cleanup", "strip", "y"}, []string{"y"}},
		{[]string{"--pathspec-from-file", "F", "y"}, []string{"y"}},
	}
	for _, c := range cases {
		if ops := splitStage("commit", c.args).ops; !slices.Equal(ops, c.ops) {
			t.Errorf("splitStage(commit, %v) ops=%v, want %v", c.args, ops, c.ops)
		}
	}
}

// TestClaimBashWritesAreClaimedAndChecked: a Bash sed -i is recorded by Claim and noted once by ClaimCheck.
func TestClaimBashWritesAreClaimedAndChecked(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "a.txt", "edited by A\n")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "sed -i s/a/b/ a.txt && echo x > /dev/null"}}))
	got := readClaims(t, claimsFile(liveOwner, "A"))
	if len(got) != 1 || got[0].Path != filepath.Join(repo, "a.txt") {
		t.Fatalf("claims = %+v; want only a.txt", got)
	}
	bash := map[string]any{"command": "sed -i s/b/c/ a.txt"}
	if r := claimCheckOn(t, repo, "B", "Bash", bash); !strings.Contains(r.Context, "a.txt has uncommitted edits by") {
		t.Fatalf("sed -i of A's dirty file: context %q; want the advisory", r.Context)
	}
	if r := claimCheckOn(t, repo, "B", "Bash", bash); r.Context != "" {
		t.Fatalf("repeat must be silent, got %q", r.Context)
	}
}

// TestClaimSkipsPathItsCommandCommitted: a path the same command committed is not claimed; a still-dirty one is.
func TestClaimSkipsPathItsCommandCommitted(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "a.txt", "A's edit\n")
	hosttest.WriteRel(t, repo, "b.txt", "A's edit\n")
	hosttest.In(t, repo, "commit", "-qm", "A lands b", "--", "b.txt")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash", "tool_input": map[string]any{
		"command": "printf x > a.txt && printf x > b.txt && git commit -qm x -- b.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "A"))
	if len(got) != 1 || got[0].Path != filepath.Join(repo, "a.txt") {
		t.Fatalf("claims = %+v; want only the still-dirty a.txt", got)
	}
}

// TestClaimCheckHintIsNonInteractive: the note names a recipe a hook shell can run, not `git add -p`.
func TestClaimCheckHintIsNonInteractive(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	note := claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": filepath.Join(repo, "a.txt")}).Context
	if !strings.Contains(note, "git apply --cached <patch>") || strings.Contains(note, "git add -p") {
		t.Errorf("note %q; want the git apply --cached recipe and no git add -p", note)
	}
}

// TestClaimCheckCleanRetiresOlderClaims: a claimed file seen clean drops claims made before; a later claim still notes.
func TestClaimCheckCleanRetiresOlderClaims(t *testing.T) {
	now := time.Now().Unix()
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(-time.Hour).Format(time.RFC3339))
	repo := claimFixture(t)
	b := filepath.Join(repo, "b.txt")
	for i, tc := range []struct {
		name       string
		peerDirty  bool
		claimAfter bool
		want       bool
	}{
		{"peer reverted before B's edit", false, false, false},
		{"peer still dirty", true, false, true},
		{"peer claims again after the clean", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaimCase(t)
			session := "B" + strconv.Itoa(i) // clean marks live in the session's seen store
			hosttest.WriteRel(t, repo, "b.txt", "b\n")
			appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now - 60}})
			if tc.peerDirty {
				hosttest.WriteRel(t, repo, "b.txt", "A's edit\n")
			}
			notes := claimCheckOn(t, repo, session, "Edit", map[string]any{"file_path": b}).Context
			hosttest.WriteRel(t, repo, "b.txt", "B's edit\n")
			if tc.claimAfter {
				appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now + 60}})
			}
			notes += claimCheckOn(t, repo, session, "Edit", map[string]any{"file_path": b}).Context
			if (notes != "") != tc.want {
				t.Fatalf("edits of b.txt: context %q; want note = %v", notes, tc.want)
			}
		})
	}
}

// Regression (2026-11-16): edits of a clean claimed file add no clean mark once one postdates every claim.
func TestClaimCheckCleanMarkOncePerClaim(t *testing.T) {
	now := time.Now().Unix()
	repo := claimFixture(t)
	b := filepath.Join(repo, "b.txt")
	appendClaims(claimsFile(liveOwner, "A"), []worktree.Claim{{Path: b, TS: now - 60}})
	if _, err := core.SeenAdd(seenKind, "B", cleanMark+b+"\t"+strconv.FormatInt(now-30, 10)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": b})
	}
	if got, _ := os.ReadFile(seenFile("B")); strings.Count(string(got), cleanMark) != 1 {
		t.Fatalf("seen store:\n%s\nwant the one clean mark, no new line per edit", got)
	}
}

// TestClaimCheckInterruptedOrCutLeafEnded: an interrupted or token-cut leaf has ended, so its claim notes nothing.
func TestClaimCheckInterruptedOrCutLeafEnded(t *testing.T) {
	const (
		work = `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`
		cut  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,` +
			`"content":"[Request interrupted by user for tool use]"}]}}`
		cutText = `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
		denied  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,` +
			`"content":"Permission to use Bash has been denied."}]}}`
		echoed = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1",` +
			`"content":[{"type":"text","text":"[Request interrupted by user]"}]}]}}`
		maxTok = `{"type":"assistant","message":{"stop_reason":"max_tokens","content":[{"type":"text","text":"x"}]}}`
		resume = `{"type":"user","isMeta":true,"message":{"content":"Output token limit hit. Resume directly"}}`
	)
	repo := claimFixture(t)
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"interrupted tool call", []string{work, cut}, false},
		{"interrupted turn", []string{work, cutText}, false},
		{"max_tokens last", []string{maxTok}, false},
		{"working", []string{work}, true},
		{"denied tool call", []string{work, denied}, true},
		{"marker in a tool's output", []string{work, echoed}, true},
		{"max_tokens being resumed", []string{maxTok, resume}, true},
		{"resumed after interrupt", []string{work, cut, work}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClaimCase(t)
			_ = os.Remove(seenFile("S")) // the advisory is once per session: each case starts unseen
			useClaimOwner(t, liveOwner)
			tr := filepath.Join(t.TempDir(), "S.jsonl")
			hosttest.WriteRel(t, filepath.Dir(tr), "S/subagents/agent-leafone1.jsonl", strings.Join(tc.lines, "\n")+"\n")
			hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
			Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
				"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "cwd": repo, "transcript_path": tr,
				"tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(repo, "a.txt")}}))
			if (r.Context != "") != tc.want {
				t.Fatalf("main edits leaf's a.txt: context %q; want note = %v", r.Context, tc.want)
			}
		})
	}
}

// The literal gates on gitStageCmd and core.GitCommitCmd change no verdict over every string in these tests.
func TestGitGatesMatchUngated(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	more, _ := filepath.Glob("../docs/*_test.go")
	n := 0
	for _, f := range append(files, more...) {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, g := range []*core.Gated{gitStageCmd, core.GitCommitCmd()} {
				if g.MatchString(s) != g.Regexp().MatchString(s) {
					t.Errorf("%s: gate changes the verdict on %q", g, s)
				}
			}
			n++
			return true
		})
	}
	if n < 100 {
		t.Fatalf("only %d test strings read, want the packages' corpus", n)
	}
}

// TestClaimCheckVerbTextIsNotACommit: the phrase as an argument is a search, never the verb, for a subagent too.
func TestClaimCheckVerbTextIsNotACommit(t *testing.T) {
	repo := claimFixture(t)
	for _, form := range []string{`rtk grep -rn "claude-code worktree commit" rules`, `grep -rn 'claude-code worktree commit' rules`,
		`rtk rg "claude-code worktree commit" .`, `echo claude-code worktree commit`, `git log --grep="claude-code worktree commit"`} {
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": form}}))
		if r.Decision != core.Allow {
			t.Errorf("subagent %q: %v %q; want allow", form, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckVerbDryRunNamesNothing: a parsed --dry-run commits nothing, in every wrapper, for a subagent too.
func TestClaimCheckVerbDryRunNamesNothing(t *testing.T) {
	repo := claimFixture(t)
	for _, form := range []string{"claude-code worktree commit --dry-run", "claude-code worktree commit -m x --dry-run",
		"rtk claude-code worktree commit --dry-run -m 'x y'", "claude-code worktree commit -dry-run=true",
		"bash -c 'claude-code worktree commit --dry-run'", `sh -c "claude-code worktree commit -m x --dry-run"`,
		"eval 'claude-code worktree commit --dry-run'", "( claude-code worktree commit --dry-run )",
		"echo $(claude-code worktree commit --dry-run)", "env X=1 claude-code worktree commit --dry-run",
		"true && /usr/local/bin/claude-code worktree commit --dry-run a.txt"} {
		r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
			"tool_name": "Bash", "tool_input": map[string]any{"command": form}}))
		if r.Decision != core.Allow {
			t.Errorf("subagent %q: %v %q; want allow", form, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckHowToOncePerSession: only a session's first advisory carries the how-to.
func TestClaimCheckHowToOncePerSession(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	claimEdit(t, repo, liveOwner, "A", "b.txt")
	edit := func(session, rel string) string {
		return claimCheckOn(t, repo, session, "Edit", map[string]any{"file_path": filepath.Join(repo, rel)}).Context
	}
	first, second, other := edit("B", "a.txt"), edit("B", "b.txt"), edit("C", "a.txt")
	for _, c := range []struct{ name, got, file string }{{"first", first, "a.txt"}, {"second", second, "b.txt"}, {"new session", other, "a.txt"}} {
		if !strings.Contains(c.got, c.file+" has uncommitted edits by claude pid ") {
			t.Errorf("%s advisory %q lacks its file, holder or time", c.name, c.got)
		}
	}
	if !strings.Contains(first, assets.HookClaimHowTo()) || strings.Contains(second, "stage just those") || !strings.Contains(other, assets.HookClaimHowTo()) {
		t.Errorf("how-to placement wrong:\nfirst  %q\nsecond %q\nnew    %q", first, second, other)
	}
}

// TestClaimCheckJudgesEveryAgentLikeClaude: Codex, Copilot and OpenCode get Claude's advisory on one claim state.
func TestClaimCheckJudgesEveryAgentLikeClaude(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n-a\n+b\n*** End Patch\n"
	type shape struct {
		agent string
		doc   map[string]any
	}
	edit := []shape{
		{core.AgentClaude, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}},
		{core.AgentCodex, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "apply_patch", "tool_input": map[string]any{"command": patch}}},
		{core.AgentCopilot, map[string]any{"sessionId": "B", "cwd": repo, "toolName": "edit", "toolArgs": map[string]any{"path": "a.txt"}}},
		{core.AgentOpencode, map[string]any{"session_id": "B", "cwd": repo, "tool_name": "Edit", "harness_agent": "opencode", "tool_input": map[string]any{"file_path": "a.txt"}}},
	}
	for name, shapes := range map[string][]shape{"edit a.txt": edit} {
		var want core.Result
		for i, s := range shapes {
			_ = os.Remove(seenFile("B")) // the advisory is once per session: each agent starts unseen
			useClaimOwner(t, checkerOwner)
			got := ClaimCheck(agentInput(t, s.agent, s.doc))
			if i == 0 {
				want = got
				if want.Context == "" {
					t.Fatalf("%s: claude's verdict %+v has no advisory; the fixture proves nothing", name, want)
				}
				continue
			}
			if got.Decision != want.Decision || got.Context != want.Context {
				t.Errorf("%s as %s: %+v, want claude's %+v", name, s.agent, got, want)
			}
		}
	}
}

// TestClaimCheckLabelsPeerByItsCommandName: a live peer is named by its process's real command, not always claude.
func TestClaimCheckLabelsPeerByItsCommandName(t *testing.T) {
	comm := proc.Comm(os.Getpid())
	if comm == "" {
		t.Skip("no process name source on this platform")
	}
	repo := claimFixture(t)
	peer := os.Getpid()
	claimAlive = func(pid int) bool { return pid == peer }
	claimEdit(t, repo, peer, "A", "a.txt")
	got := claimCheckOn(t, repo, "B", "Edit", map[string]any{"file_path": filepath.Join(repo, "a.txt")}).Context
	if want := "a.txt has uncommitted edits by " + comm + " pid "; !strings.Contains(got, want) {
		t.Errorf("advisory %q lacks %q", got, want)
	}
}

// lanesCases are the lanes-mode claim check's rows: mode, file, command, in a lane, agent id and whether it notes.
var lanesCases = []struct {
	name, mode, file, cmd string
	lane                  bool
	agent                 string
	note                  bool
}{
	{"live checkout edit notes main", "lanes", "a.txt", "", false, "", true},
	{"live checkout edit notes a leaf", "lanes", "a.txt", "", false, "leaftwo2", true},
	{"live checkout stage notes main", "lanes", "a.txt", "git add a.txt", false, "", true},
	{"untracked file is free", "lanes", "new.txt", "", false, "", false},
	{"same path in a lane is free", "lanes", "a.txt", "", true, "leaftwo2", false},
	{"stage in a lane is free", "lanes", "a.txt", "git add a.txt", true, "", false},
	{"not lanes mode", "", "a.txt", "", false, "leaftwo2", false},
	{"apply notes main", "lanes", "a.txt", "git apply p.patch", false, "", true},
	{"apply --index notes a leaf", "lanes", "a.txt", "git apply --index p.patch", false, "leaftwo2", true},
	{"am notes a leaf", "lanes", "a.txt", "git am p.mbox", false, "leaftwo2", true},
	{"mv of a tracked file notes main", "lanes", "a.txt", "git mv a.txt c.txt", false, "", true},
	{"mv of a tracked file notes a leaf", "lanes", "a.txt", "git mv a.txt c.txt", false, "leaftwo2", true},
	{"add -p notes main", "lanes", "a.txt", "git add -p a.txt", false, "", true},
	{"add --patch notes a leaf", "lanes", "a.txt", "git add --patch a.txt", false, "leaftwo2", true},
	{"apply --check passes", "lanes", "a.txt", "git apply --check p.patch", false, "leaftwo2", false},
	{"apply --stat passes", "lanes", "a.txt", "git apply --stat p.patch", false, "leaftwo2", false},
	{"apply --numstat passes", "lanes", "a.txt", "git apply --numstat p.patch", false, "leaftwo2", false},
	{"mv -n passes", "lanes", "a.txt", "git mv -n a.txt c.txt", false, "leaftwo2", false},
	{"add -n passes", "lanes", "a.txt", "git add -n a.txt", false, "leaftwo2", false},
	{"apply in a lane is free", "lanes", "a.txt", "git apply p.patch", true, "leaftwo2", false},
	{"not lanes apply", "", "a.txt", "git apply p.patch", false, "leaftwo2", false},
	{"not lanes mv", "", "a.txt", "git mv a.txt c.txt", false, "leaftwo2", false},
	{"not lanes add -p", "", "a.txt", "git add -p a.txt", false, "leaftwo2", false},
}

// TestClaimCheckLanesMode: lanes mode refuses tracked edits and stages in the live checkout only; a lane stays open.
func TestClaimCheckLanesMode(t *testing.T) {
	for _, tc := range lanesCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			t.Setenv("CLAUDE_CONFIG_DIR", repo)
			t.Setenv(worktreeModeEnv, tc.mode)
			root := repo
			if tc.lane {
				root = filepath.Join(paths.Real(t.TempDir()), "lane")
				hosttest.In(t, repo, "worktree", "add", "-q", "-b", "lane", root)
			}
			hosttest.WriteRel(t, root, tc.file, "edited\n")
			in := map[string]any{"session_id": "S", "cwd": root, "agent_id": tc.agent, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": filepath.Join(root, tc.file)}}
			if tc.cmd != "" {
				in["tool_name"], in["tool_input"] = "Bash", map[string]any{"command": tc.cmd}
			}
			useClaimOwner(t, checkerOwner)
			r := ClaimCheck(claimInput(t, in))
			if r.Decision != core.Allow {
				t.Fatalf("%v %q; lanes mode never asks or blocks", r.Decision, r.Reason)
			}
			if noted := strings.Contains(r.Context, "claude-code worktree create "); noted != tc.note {
				t.Errorf("context %q: lane note %v, want %v", r.Context, noted, tc.note)
			}
			if strings.Contains(r.Context, "<") {
				t.Errorf("context %q keeps a placeholder", r.Context)
			}
		})
	}
}

// TestClaimCheckLanesGitFailureWarns: lanes mode fails open when git breaks or the budget ends, with a warning.
func TestClaimCheckLanesGitFailureWarns(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T)
	}{
		{"git fails", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("shell shim")
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}},
		{"budget expired", func(t *testing.T) { hookBudget(t, "worktree-claim-check", time.Nanosecond) }},
	} {
		for _, cmd := range []string{"", "git add a.txt"} {
			t.Run(tc.name+" "+cmd, func(t *testing.T) {
				repo := claimFixture(t)
				t.Setenv("CLAUDE_CONFIG_DIR", repo)
				t.Setenv(worktreeModeEnv, "lanes")
				hosttest.WriteRel(t, repo, "a.txt", "edited\n")
				in := map[string]any{"session_id": "S", "cwd": repo, "tool_name": "Edit",
					"tool_input": map[string]any{"file_path": filepath.Join(repo, "a.txt")}}
				if cmd != "" {
					in["tool_name"], in["tool_input"] = "Bash", map[string]any{"command": cmd}
				}
				tc.arm(t)
				r := ClaimCheck(claimInput(t, in))
				if r.Decision != core.Allow || !strings.Contains(r.Stderr, "could not check lanes-mode protection") {
					t.Fatalf("%v %q; want allow with the lanes warning", r.Decision, r.Stderr)
				}
			})
		}
	}
}

// Another repo notes until an edit lands (Claim), never in a lane; it never asks, for any agent.
func TestClaimCheckLanesOtherRepoAsksOnce(t *testing.T) {
	other := claimFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "lanes")
	lane := filepath.Join(paths.Real(t.TempDir()), "lane")
	hosttest.In(t, other, "worktree", "add", "-q", "-b", "lane", lane)
	useClaimOwner(t, checkerOwner)
	check := func(root, session, agent string) core.Result {
		in := map[string]any{"session_id": session, "cwd": root, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(root, "a.txt")}, "agent_id": map[string]string{"LEAF": "a1"}[session]}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return ClaimCheck(core.ParseInputFor(strings.NewReader(string(b)), agent))
	}
	for i := range 2 { // the note repeats until an edit lands
		if r := check(other, "S1", core.AgentClaude); r.Decision != core.Allow || !strings.Contains(r.Context, "of "+other+" change in its main checkout") {
			t.Fatalf("edit %d before Claim: %v %q; want allow with a note", i+1, r.Decision, r.Context)
		}
	}
	Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": other, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(other, "a.txt")}}))
	for name, r := range map[string]core.Result{"second edit": check(other, "S1", core.AgentClaude), "linked worktree": check(lane, "S2", core.AgentClaude)} {
		if r.Decision != core.Allow {
			t.Errorf("%s: %v %q; want allow", name, r.Decision, r.Reason)
		}
	}
	for name, r := range map[string]core.Result{"claude leaf": check(other, "LEAF", core.AgentClaude), "codex": check(other, "S3", "codex")} {
		if r.Decision != core.Allow || r.Context == "" {
			t.Errorf("%s: %v, context %q; want allow with a note", name, r.Decision, r.Context)
		}
	}
}

func TestClaimEditSkipsGitAndCleanPeerCheckUsesIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := claimFixture(t)
	peerFile := filepath.Join(repo, "shared.txt")
	claimEdit(t, repo, liveOwner, "sess-a", "shared.txt")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(peerFile, old, old); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, repo, "add", "shared.txt")
	hosttest.In(t, repo, "commit", "-qm", "commit shared")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	useClaimOwner(t, checkerOwner)
	hosttest.WriteRel(t, repo, "a.txt", "dirty\n")
	Claim(claimInput(t, map[string]any{"session_id": "sess-b", "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(repo, "a.txt")}}))
	if _, err := os.Stat(claimsFile(checkerOwner, "sess-b")); err != nil {
		t.Fatalf("Edit claim did not record without git: %v", err)
	}
	if r := ClaimCheck(claimInput(t, map[string]any{"session_id": "sess-b", "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": peerFile}})); r.Decision != core.Allow || r.Context != "" || r.Stderr != "" {
		t.Fatalf("clean peer check with broken git = %+v, want allow with no warning", r)
	}
}

func BenchmarkClaimCheckHandler(b *testing.B) {
	for _, mode := range []string{"SeenRepo", "Peer"} {
		b.Run(mode, func(b *testing.B) {
			repo := benchClaimRepo(b)
			orig := claimOwner
			claimOwner = func() int { return checkerOwner }
			b.Cleanup(func() { claimOwner = orig })
			if _, err := core.SeenAdd(laneSeenKind, "bench", repo); err != nil {
				b.Fatal(err)
			}
			if mode == "Peer" {
				appendClaims(claimsFile(os.Getpid(), "peer"), []worktree.Claim{
					{Path: filepath.Join(repo, "file-000.txt"), TS: time.Now().Unix()},
				})
			}
			in := claimInput(b, map[string]any{"session_id": "bench", "cwd": repo, "tool_name": "Edit",
				"tool_input": map[string]any{"file_path": "file-000.txt"}})
			ClaimCheck(in)
			b.ReportAllocs()
			for b.Loop() {
				ClaimCheck(in)
			}
		})
	}
}

func TestOtherVerdictSeenEditSkipsOnlyReadOnlyGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	repo := claimFixture(t)
	t.Setenv(worktreeModeEnv, "lanes")
	if _, err := core.SeenAdd(laneSeenKind, "S", repo); err != nil {
		t.Fatal(err)
	}
	bin, trace := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + strings.ReplaceAll(trace, "'", "'\\''") + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cmd := range []string{"", "sed -i s/a/b/ a.txt", "git add a.txt", "git mv a.txt b.txt"} {
		_ = os.Remove(trace)
		tool := "Bash"
		if cmd == "" {
			tool = "Edit"
		}
		in := claimInput(t, map[string]any{"session_id": "S", "cwd": repo, "tool_name": tool,
			"tool_input": map[string]any{"command": cmd, "file_path": "a.txt"}})
		got := otherVerdict(t.Context(), in, cmd, editPaths(in), paths.Real(paths.ConfigDir()))
		if !reflect.DeepEqual(got, core.Result{}) {
			t.Fatalf("%q: result %+v", cmd, got)
		}
		raw, _ := os.ReadFile(trace)
		if ran := len(raw) > 0; ran != laneText(cmd) {
			t.Fatalf("%q: git calls %q; staging/move checks must still run", cmd, raw)
		}
	}
}

func TestMarkLaneReposPreservesSeenBytes(t *testing.T) {
	repo := claimFixture(t)
	t.Setenv(worktreeModeEnv, "lanes")
	in := claimInput(t, map[string]any{"session_id": "S", "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}})
	file := core.SeenFile(laneSeenKind, "S")
	if _, err := core.SeenAdd(laneSeenKind, "S", "previous"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		markLaneRepos(in, editPaths(in))
		if raw, _ := os.ReadFile(file); string(raw) != "previous\n"+repo+"\n" {
			t.Fatalf("seen bytes = %q", raw)
		}
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	markLaneRepos(in, editPaths(in))
	if raw, _ := os.ReadFile(file); string(raw) != repo+"\n" {
		t.Fatalf("recreated seen bytes = %q", raw)
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(file) {
		t.Fatalf("seen directory = %v, %v", entries, err)
	}
}
