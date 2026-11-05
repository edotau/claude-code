package wthook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/notices"
	"github.com/edotau/claude-code/internal/worktree"
)

// divergedClone is a clone whose main has one local commit (claimed by session peerS) and one new upstream commit.
func divergedClone(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "off")
	origin := gittest.Repo(t)
	work := filepath.Join(t.TempDir(), "work")
	gittest.In(t, origin, "clone", "-q", origin, work)
	work = core.CanonPath(work)
	gittest.WriteRel(t, origin, "up.txt", "up\n")
	gittest.In(t, origin, "add", ".")
	gittest.In(t, origin, "commit", "-qm", "upstream")
	gittest.WriteRel(t, work, "mine.txt", "mine\n")
	gittest.In(t, work, "add", ".")
	gittest.In(t, work, "commit", "-qm", "local")
	gittest.In(t, work, "fetch", "-q")
	short := strings.TrimSpace(gittest.In(t, work, "rev-parse", "--short=7", "HEAD"))
	if err := os.MkdirAll(claimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	gittest.WriteRel(t, claimsDir(), filepath.Base(claimsFile(liveOwner, "peerS")), `{"schema":1,"commit":"`+short+`","ts":1}`+"\n")
	orig, origPeers := claimAlive, PeerAgents
	claimAlive = func(pid int) bool { return pid == liveOwner }
	PeerAgents = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	t.Cleanup(func() { claimAlive, PeerAgents = orig, origPeers })
	return work
}

// TestNoticesAndDivergenceReachContextOnce: a shared diverged checkout and a queued notice each surface once.
func TestNoticesAndDivergenceReachContextOnce(t *testing.T) {
	work := divergedClone(t)
	in := &core.Input{SessionID: "sB", CWD: work}
	got := Busy(in)
	if !strings.Contains(got.Context, "local main diverged from origin/main (ahead 1, behind 1)") ||
		!strings.Contains(got.Context, "ahead: session peers (pid 100)") || got.Stderr != "" {
		t.Fatalf("first prompt = %+v, want the divergence line naming the ahead commit's session", got)
	}
	if again := Busy(in); again.Context != "" {
		t.Fatalf("second prompt = %q, want the divergence line only once", again.Context)
	}
	if err := notices.Append("sB", notices.Notice{Kind: "conflict", From: "worktree merge", Text: "x.go"}); err != nil {
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
