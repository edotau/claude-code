package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claimFixture is a repo with a.txt and b.txt committed, a config dir, and liveOwner as the only live pid.
func claimFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := canonPath(gitRepo(t))
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

func claimCheckOn(t *testing.T, repo, session, tool string, input map[string]any) Result {
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
	if r.Decision != Ask || !strings.Contains(r.Reason, "a.txt takes") || strings.Contains(r.Reason, "b.txt") {
		t.Fatalf("git add -A: %v %q; want an ask naming a.txt only (b.txt's owner is dead)", r.Decision, r.Reason)
	}
	if _, err := os.Stat(claimsFile(deadOwner, "D")); !os.IsNotExist(err) {
		t.Fatalf("dead owner's claims file must be removed, stat err = %v", err)
	}
	for _, cmd := range []string{"git add -p a.txt", "git add b.txt", "git status"} {
		if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": cmd}); r.Decision != Allow {
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
	if r.Decision != Block || !strings.Contains(r.Reason, "leaf leafone1") {
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
	if r := claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git add a.txt"}); r.Decision != Allow {
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
	if r.Decision != Ask || !strings.Contains(r.Reason, "b.txt takes") {
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
	if r.Decision != Ask || !strings.Contains(r.Reason, "takes 3 file(s), 1 of them") {
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
	if r.Decision != Ask || !strings.Contains(r.Reason, "a.txt takes") || !safe {
		t.Fatalf("add && bare commit: %v %q; want an ask naming a.txt and git commit --", r.Decision, r.Reason)
	}
	if r = claimCheckOn(t, repo, "B", "Bash", map[string]any{"command": "git commit -m x -- b.txt"}); r.Decision != Allow {
		t.Fatalf("commit -- b.txt: %v %q; want allow", r.Decision, r.Reason)
	}
}
