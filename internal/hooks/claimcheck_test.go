package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

// claimFixture is a repo with a.txt and b.txt committed, a config dir, and liveOwner as the only live pid.
func claimFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := core.CanonPath(gitRepo(t))
	writeRel(t, repo, "a.txt", "a\n")
	writeRel(t, repo, "b.txt", "b\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "files")
	orig := claimAlive
	claimAlive = func(pid int) bool { return pid == liveOwner }
	t.Cleanup(func() { claimAlive = orig })
	return repo
}

// claimEdit records an Edit of rel by session (owner pid) and leaves rel dirty.
func claimEdit(t *testing.T, repo string, owner int, session, rel string) {
	t.Helper()
	useClaimOwner(t, owner)
	writeRel(t, repo, rel, "edited by "+session+"\n")
	worktreeClaim(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": rel}}))
}

func claimCheckOn(t *testing.T, repo, session, tool string, input map[string]any) core.Result {
	t.Helper()
	return worktreeClaimCheck(claimInput(t, map[string]any{"session_id": session, "cwd": repo, "tool_name": tool,
		"tool_input": input}))
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

// TestClaimCheckLeafBlocksOnSiblingClaim: a leaf staging a file a live sibling leaf of its session claims is blocked.
func TestClaimCheckLeafBlocksOnSiblingClaim(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	tr := filepath.Join(t.TempDir(), "S.jsonl")
	writeRel(t, filepath.Dir(tr), "S/subagents/agent-leafone1.jsonl", "{}\n")
	writeRel(t, repo, "a.txt", "leaf one\n")
	worktreeClaim(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leafone1", "cwd": repo,
		"transcript_path": tr, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	r := worktreeClaimCheck(claimInput(t, map[string]any{"session_id": "S", "agent_id": "leaftwo2", "cwd": repo,
		"transcript_path": tr, "tool_name": "Bash", "tool_input": map[string]any{"command": "git commit -am x"}}))
	if r.Decision != core.Block || !strings.Contains(r.Reason, "leaf leafone1") {
		t.Fatalf("sibling leaf commit -am: %v %q; want a block naming leaf leafone1", r.Decision, r.Reason)
	}
}

// TestClaimCheckDropsLandedClaim: a claim older than the file's last commit has landed, so staging the file passes.
func TestClaimCheckDropsLandedClaim(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	gitIn(t, repo, "add", "a.txt")
	t.Setenv("GIT_COMMITTER_DATE", time.Now().Add(time.Hour).Format(time.RFC3339))
	gitIn(t, repo, "commit", "-qm", "A lands a.txt")
	writeRel(t, repo, "a.txt", "B's own edit\n")
	if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"}); r.Decision != core.Allow {
		t.Fatalf("landed claim: %v %q; want allow", r.Decision, r.Reason)
	}
}

// TestClaimCheckBashEditClaimedByAdd: a file changed outside Edit is claimed once staged; a peer's bare commit asks.
func TestClaimCheckBashEditClaimedByAdd(t *testing.T) {
	repo := claimFixture(t)
	useClaimOwner(t, liveOwner)
	writeRel(t, repo, "b.txt", "sed -i by A\n")
	gitIn(t, repo, "add", "b.txt")
	worktreeClaim(claimInput(t, map[string]any{"session_id": "A", "cwd": repo, "tool_name": "Bash",
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
	writeRel(t, repo, "c.txt", "unclaimed\n")
	writeRel(t, repo, "d.txt", "unclaimed\n")
	gitIn(t, repo, "add", "a.txt", "c.txt", "d.txt")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x"})
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "takes 3 file(s), 1 of them") {
		t.Fatalf("bare commit of 3 staged, 1 claimed: %v %q; want the ask to count 3 and 1", r.Decision, r.Reason)
	}
}

// TestClaimCheckCommitAskNamesSafeForm: add-then-bare-commit over a peer's staged file asks, naming `git commit --`.
func TestClaimCheckCommitAskNamesSafeForm(t *testing.T) {
	repo := claimFixture(t)
	claimEdit(t, repo, liveOwner, "A", "a.txt")
	gitIn(t, repo, "add", "a.txt")
	writeRel(t, repo, "b.txt", "B's own\n")
	r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add b.txt && git commit -q -m x"})
	safe := strings.Contains(r.Reason, "git commit -- <your files>")
	if r.Decision != core.Ask || !strings.Contains(r.Reason, "a.txt takes") || !safe {
		t.Fatalf("add && bare commit: %v %q; want an ask naming a.txt and git commit --", r.Decision, r.Reason)
	}
	if r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- b.txt"}); r.Decision != core.Allow {
		t.Fatalf("commit -- b.txt: %v %q; want allow", r.Decision, r.Reason)
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
		if _, all := stageOperands(c.verb, c.args); all != c.all {
			t.Errorf("stageOperands(%s, %v) all=%v, want %v", c.verb, c.args, all, c.all)
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
	}
	for _, c := range cases {
		if ops, _ := stageOperands("commit", c.args); !slices.Equal(ops, c.ops) {
			t.Errorf("stageOperands(commit, %v) ops=%v, want %v", c.args, ops, c.ops)
		}
	}
}

func writeRel(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
