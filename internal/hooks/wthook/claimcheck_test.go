package wthook

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// claimFixture is a repo with a.txt and b.txt committed, a config dir, and liveOwner as the only live pid.
func claimFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := core.CanonPath(hosttest.Repo(t))
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

func claimCheckOn(t *testing.T, repo, session, tool string, input map[string]any) core.Result {
	t.Helper()
	useClaimOwner(t, checkerOwner) // the check runs as a process other than the claimants
	return ClaimCheck(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": tool,
		"tool_input": input}))
}

func TestClaimCheckGitPathspecs(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for _, args := range []string{"'" + filepath.Join(repo, "a.txt") + "'", "'*.txt'", "':(top)a.txt'"} {
		r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -- " + args})
		if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") {
			t.Errorf("git add -- %s: %v %q; want an ask for a.txt", args, r.Decision, r.Reason)
		}
	}
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -- . ':(exclude)a.txt'"})
	if r.Decision != core.Allow {
		t.Fatalf("excluded peer file: %v %q; want allow", r.Decision, r.Reason)
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
	r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- a.txt"})
	if r.Decision != core.Ask {
		t.Fatalf("commit of staged deletion: %v %q; want ask", r.Decision, r.Reason)
	}
}

func TestClaimCheckQuotedStagedPath(t *testing.T) {
	repo := claimFixture(t)
	const name = "caf\u00e9.txt"
	hosttest.WriteRel(t, repo, name, "external edit\n")
	hosttest.In(t, repo, "-c", "core.quotePath=true", "add", "--", name)
	useClaimOwner(t, liveOwner)
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add -- ."}}))
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git -c core.quotePath=true commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, name+" takes") {
		t.Fatalf("bare commit of quoted filename: %v %q; want an ask for %s", r.Decision, r.Reason, name)
	}
}

// TestClaimCheck: a live peer's dirty claim advises once and asks on staging; a dead owner's claim and file go.
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
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add -A"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") || strings.Contains(r.Reason, "b.txt") {
		t.Fatalf("git add -A: %v %q; want an ask naming a.txt only (b.txt's owner is dead)", r.Decision, r.Reason)
	}
	if _, err := os.Stat(claimsFile(deadOwner, "D")); !os.IsNotExist(err) {
		t.Fatalf("dead owner's claims file must be removed, stat err = %v", err)
	}
	for _, cmd := range []string{"git add -p a.txt", "git add b.txt", "git status"} {
		if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": cmd}); r.Decision != core.Allow {
			t.Fatalf("%s: %v %q; want allow", cmd, r.Decision, r.Reason)
		}
	}
}

// TestClaimCheckOwnPidNewSlug: after /clear or a resume the same pid files under a new slug; that is no peer.
func TestClaimCheckOwnPidNewSlug(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checkPid  int
		wantAsked bool
	}{{"same pid, new session", liveOwner, false}, {"other pid, other session", checkerOwner, true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "OLD", "a.txt")
			useClaimOwner(t, tc.checkPid)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "NEW", "cwd": repo, "tool_name": "Bash",
				"tool_input": map[string]any{"command": "git add -A"}}))
			if asked := r.Decision == core.Ask; asked != tc.wantAsked {
				t.Fatalf("git add -A as pid %d: %v %q; want ask = %v", tc.checkPid, r.Decision, r.Reason, tc.wantAsked)
			}
		})
	}
}

// TestClaimCheckLeafBlocksOnSiblingClaim: a leaf staging a file a live sibling leaf of its session claims is blocked.
func TestClaimCheckLeafBlocksOnSiblingClaim(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	tr := filepath.Join(t.TempDir(), "S.jsonl")
	hosttest.WriteRel(t, filepath.Dir(tr), "S/subagents/agent-leafone1.jsonl", "{}\n")
	hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
	Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
		"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	r := ClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
		"transcript_path": tr, "tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -am x"}}))
	if r.Decision != core.Block || !strings.Contains(r.Reason, "leaf leafone1") {
		t.Fatalf("sibling leaf commit -am: %v %q; want a block naming leaf leafone1", r.Decision, r.Reason)
	}
}

// TestClaimCheckMainCommitsHandedBackLeaf: main asks on a live leaf's claim and a denied hand-back, passes once delivered.
func TestClaimCheckMainCommitsHandedBackLeaf(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	tr := filepath.Join(t.TempDir(), "S.jsonl")
	leafTr := "S/subagents/agent-leafone1.jsonl"
	working := `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"Edit"}]}}` + "\n"
	hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working)
	hosttest.WriteRel(t, repo, "a.txt", "leaf one\n")
	Claim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
		"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	add := map[string]any{"session_id": "S", "cwd": repo, "transcript_path": tr, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add a.txt"}}
	if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Ask {
		t.Fatalf("live leaf's file: %v %q; want ask", r.Decision, r.Reason)
	}
	handback := `{"type":"assistant","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_h1",` +
		`"name":"SubagentHandback"}]}}` + "\n"
	denied := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"PreToolUse:` +
		`SubagentHandback hook error: dispatch: BLOCKED — job running","is_error":true,"tool_use_id":"toolu_h1"}]},` +
		`"toolUseResult":"Error: PreToolUse:SubagentHandback hook error","toolDenialKind":"permission-rule"}` + "\n"
	for name, tail := range map[string]string{"pending": "", "denied": denied} {
		hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working+handback+tail)
		if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Ask {
			t.Fatalf("%s hand-back leaf's file: %v %q; want ask", name, r.Decision, r.Reason)
		}
	}
	delivered := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_h1","type":"tool_result",` +
		`"content":[{"type":"text","text":"{\"success\":true}"}]}]},"toolUseResult":{"success":true},"toolEndsTurn":true}` + "\n"
	hosttest.WriteRel(t, filepath.Dir(tr), leafTr, working+handback+delivered)
	if r := ClaimCheck(claimInput(t, add)); r.Decision != core.Allow {
		t.Fatalf("handed-back leaf's file: %v %q; want allow", r.Decision, r.Reason)
	}
}

// TestClaimCheckDropsLandedClaim: a claim older than the file's last commit has landed, so staging the file passes.
func TestClaimCheckDropsLandedClaim(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.In(t, repo, "add", "a.txt")
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Hour).Format(time.RFC3339))
	hosttest.In(t, repo, "commit", "-qm", "A lands a.txt")
	hosttest.WriteRel(t, repo, "a.txt", "B's own edit\n")
	if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"}); r.Decision != core.Allow {
		t.Fatalf("landed claim: %v %q; want allow", r.Decision, r.Reason)
	}
}

// TestClaimCheckBashEditClaimedByAdd: a file changed outside Edit is claimed once staged; a peer's bare commit asks.
func TestClaimCheckBashEditClaimedByAdd(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	hosttest.WriteRel(t, repo, "b.txt", "sed -i by A\n")
	hosttest.In(t, repo, "add", "b.txt")
	Claim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "git add b.txt"}, "tool_response": map[string]any{"stdout": ""}}))
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "b.txt takes") {
		t.Fatalf("bare commit over A's staged b.txt: %v %q; want an ask naming b.txt", r.Decision, r.Reason)
	}
}

// The commit ask sizes the sweep: every staged file it takes, and how many of those a peer claims.
func TestClaimCheckCommitAskCountsWholeStage(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.WriteRel(t, repo, "c.txt", "unclaimed\n")
	hosttest.WriteRel(t, repo, "d.txt", "unclaimed\n")
	hosttest.In(t, repo, "add", "a.txt", "c.txt", "d.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "takes 3 file(s), 1 of them") {
		t.Fatalf("bare commit of 3 staged, 1 claimed: %v %q; want the ask to count 3 and 1", r.Decision, r.Reason)
	}
}

// TestClaimCheckCommitAskNamesSafeForm: add-then-bare-commit over a peer's staged file asks, naming `git commit --`.
func TestClaimCheckCommitAskNamesSafeForm(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.WriteRel(t, repo, "b.txt", "B's own\n")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add b.txt && git commit -q -m x"})
	safe := strings.Contains(r.Reason, "git commit -- <your files>")
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") || !safe {
		t.Fatalf("add && bare commit: %v %q; want an ask naming a.txt and git commit --", r.Decision, r.Reason)
	}
	if r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- b.txt"}); r.Decision != core.Allow {
		t.Fatalf("commit -- b.txt: %v %q; want allow", r.Decision, r.Reason)
	}
}

func TestClaimCheckCommitInteractiveFlags(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    core.Decision
	}{
		{"git commit -i -m x -- a.txt", core.Block},
		{"git commit -m '-p' -- a.txt", core.Block},
		{"git commit -p -m x -- a.txt", core.Allow},
		{"git add -i -- a.txt", core.Allow},
	} {
		t.Run(tc.command, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": tc.command}}))
			if r.Decision != tc.want {
				t.Fatalf("%s: %v %q; want %v", tc.command, r.Decision, r.Reason, tc.want)
			}
			if tc.want == core.Block && !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("block must name claimed a.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckCommitIncludeRetainsStaged(t *testing.T) {
	for _, flag := range []string{"--include", "-i"} {
		t.Run(flag, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.In(t, repo, "add", "a.txt")
			hosttest.WriteRel(t, repo, "b.txt", "B's own edit\n")
			command := "git commit " + flag + " -m x -- b.txt"
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": command}}))
			if r.Decision != core.Block || !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("%s: %v %q; want block naming staged a.txt", command, r.Decision, r.Reason)
			}
			if !strings.Contains(r.Reason, "takes 2 file(s), 1 of them") {
				t.Fatalf("include must count staged a.txt and operand b.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckAddPathspecFromFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		option  string
		content string
		want    core.Decision
	}{
		{"lines", "--pathspec-from-file paths.txt", "a.txt\n", core.Block},
		{"equals", "--pathspec-from-file=paths.txt", "a.txt\n", core.Block},
		{"quoted", "--pathspec-from-file paths.txt", "\"a.txt\"\r\n", core.Block},
		{"nul", "--pathspec-from-file paths.txt --pathspec-file-nul", "a.txt\x00", core.Block},
		{"unclaimed", "--pathspec-from-file paths.txt", "b.txt\n", core.Allow},
		{"empty", "--pathspec-from-file paths.txt", "", core.Allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.WriteRel(t, repo, "b.txt", "B's own edit\n")
			hosttest.WriteRel(t, repo, "paths.txt", tc.content)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": "git add " + tc.option}}))
			if r.Decision != tc.want {
				t.Fatalf("%s: %v %q; want %v", tc.option, r.Decision, r.Reason, tc.want)
			}
			if tc.want == core.Block && !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("block must name claimed a.txt, got %q", r.Reason)
			}
		})
	}
}

func TestClaimCheckCommitPathspecFromFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		option  string
		content string
	}{
		{"lines", "--pathspec-from-file paths.txt", "a.txt\n"},
		{"equals", "--pathspec-from-file=paths.txt", "a.txt\n"},
		{"quoted", "--pathspec-from-file paths.txt", "\"a.txt\"\r\n"},
		{"nul", "--pathspec-from-file paths.txt --pathspec-file-nul", "a.txt\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := claimFixture(t)
			claimEdit(t, repo, liveOwner, "A", "a.txt")
			hosttest.WriteRel(t, repo, "paths.txt", tc.content)
			r := ClaimCheck(claimInput(t, map[string]any{"session_id": "B", "agent_id": "leafB", "cwd": repo,
				"tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -m x " + tc.option}}))
			if r.Decision != core.Block || !strings.Contains(r.Reason, "a.txt takes") {
				t.Fatalf("%s: %v %q; want block naming unstaged a.txt", tc.option, r.Decision, r.Reason)
			}
		})
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

// TestClaimCheckNestedStaging: git add/commit inside bash -c or a substitution still reaches the stage check.
func TestClaimCheckNestedStaging(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	for _, cmd := range []string{"bash -c 'git add -A'", "echo $(git commit -a -m x)", "timeout -s KILL 5 git add -A"} {
		r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": cmd})
		if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") {
			t.Errorf("%s: %v %q; want an ask naming a.txt", cmd, r.Decision, r.Reason)
		}
	}
}

func TestClaimCheckNestedTooDeepAsks(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "eval eval eval eval eval git add -A"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "nests too deep") {
		t.Errorf("%v %q; want an ask naming the nesting", r.Decision, r.Reason)
	}
}

// TestFirstSightingLeavesNoLockFile: the seen file locks itself, so no .lock sibling is left behind.
func TestFirstSightingLeavesNoLockFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	firstSighting("sess", "k1")
	dir := filepath.Join(paths.StateDir(), "hooks", "claims-seen")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || strings.HasSuffix(ents[0].Name(), ".lock") {
		t.Fatalf("claims-seen dir = %v (err %v); want only the seen file", ents, err)
	}
}

// TestFirstSightingFailsOpenOnHeldLock: a held lock costs at most claimsSeenWait, then the advisory still shows.
func TestFirstSightingFailsOpenOnHeldLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := filepath.Join(paths.StateDir(), "hooks", "claims-seen", "sess")
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
	start := time.Now()
	if !firstSighting("sess", "k1") || time.Since(start) > 5*time.Second {
		t.Fatalf("held lock: want fail-open true within the bounded wait, took %v", time.Since(start))
	}
}

// TestStageOperandsAttachedValueTakesNoNextWord: git reads `-mc x` as message "c" with pathspec x, and `-um x` as -u<mode m>.
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
