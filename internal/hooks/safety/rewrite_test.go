package safety

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/worktree"
)

// rewriteFixture builds a bare origin, a clone with two pushed commits, and a linked worktree on a pushed branch.
func rewriteFixture(t *testing.T) (work, wt string) {
	t.Helper()
	root := paths.Real(t.TempDir())
	work, wt = filepath.Join(root, "work"), filepath.Join(root, "wt")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "-c", "init.defaultBranch=main", "init", "-q", "--bare", "origin.git")
	run(root, "clone", "-q", "origin.git", "work")
	for _, msg := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(work, msg+".txt"), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		run(work, "add", ".")
		run(work, "commit", "-qm", msg)
	}
	run(work, "push", "-qu", "origin", "HEAD:main")
	run(work, "worktree", "add", "-q", "-b", "feat", wt)
	run(wt, "push", "-qu", "origin", "feat")
	solo := strings.TrimSpace(hosttest.In(t, work, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "solo"))
	hosttest.In(t, work, "branch", "solo", solo) // a branch holding one commit no other ref reaches
	return work, wt
}

// peerPID is the fake peer the swapped worktree.PeerScan seam reports.
const peerPID = 4242

type rewriteCase struct {
	name, cmd, cwd, peerIn, agentID, agent string
	live, leaves, noGit, homeHolds         bool
	want                                   core.Decision
	text                                   string
}

// rewriteSetup applies one case's environment: config dir, peer seam, leaf marker, agent and PATH.
func rewriteSetup(t *testing.T, c rewriteCase, dirs map[string]string) {
	t.Helper()
	cfg := t.TempDir()
	if c.live {
		cfg = dirs["work"]
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv(settings.AgentEnv, c.agent)
	writeSessionJSON(t, cfg)
	orig := worktree.PeerScan
	worktree.PeerScan = func(top string, _ []string) ([]worktree.Proc, error) {
		if c.peerIn != "" && top == dirs[c.peerIn] {
			return []worktree.Proc{{PID: peerPID, Comm: "claude"}}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { worktree.PeerScan = orig })
	if c.leaves {
		seedLeafMarker(t)
	}
	if c.noGit {
		t.Setenv("PATH", t.TempDir())
	}
	if c.homeHolds { // the fixture then sits under a live root, so no temp-folder allowance applies
		t.Setenv("HOME", filepath.Dir(dirs["work"]))
	}
}

func writeSessionJSON(t *testing.T, cfg string) {
	t.Helper()
	dir := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"sessionId":"abcdef0123456789","name":"peer-a"}`)
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(peerPID)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedLeafMarker writes one live inflight marker owned by this session's owner.
func seedLeafMarker(t *testing.T) {
	t.Helper()
	owner := inflight.SessionOwner()
	if owner == 0 {
		t.Skip("session owner unavailable")
	}
	name := inflight.Prefix + strconv.Itoa(owner) + "-toolu_leaf"
	if err := os.MkdirAll(inflight.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inflight.Dir(), name), []byte("acct\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rewriteRefCases are the branch-ref moves and GIT_DIR spellings of a rewrite.
func rewriteRefCases() []rewriteCase {
	return []rewriteCase{
		{name: "update-ref move in the live tree", cmd: "git update-ref refs/heads/solo HEAD", cwd: "work", live: true,
			want: core.Block, text: "live config tree"},
		{name: "update-ref -d in the live tree", cmd: "git update-ref -d refs/heads/solo", cwd: "work", live: true,
			want: core.Block, text: "live config tree"},
		{name: "GIT_DIR names the live tree", cmd: "GIT_DIR=WORK/.git git reset --hard HEAD~1", cwd: "wt", live: true,
			want: core.Block, text: "live config tree"},
		{name: "branch -f in an unshared clone", cmd: "git branch -f solo HEAD", cwd: "work", want: core.Allow},
		{name: "branch -D in an own worktree", cmd: "git branch -D feat", cwd: "wt", want: core.Allow},
		{name: "branch -D of a merged branch", cmd: "git branch -D feat", cwd: "work", live: true, want: core.Allow},
		{name: "update-ref -d of a merged branch", cmd: "git update-ref -d refs/heads/feat", cwd: "work", live: true,
			want: core.Allow},
		{name: "branch --delete is -d", cmd: "git branch --delete solo", cwd: "work", live: true, want: core.Allow},
		{name: "live leaves, repo outside the config tree", cmd: "git commit --amend --no-edit", cwd: "work", leaves: true,
			want: core.Allow},
		{name: "live leaves and a peer outside the tree", cmd: "git commit --amend --no-edit", cwd: "work", leaves: true,
			peerIn: "work", want: core.Block, text: "works in it"},
		{name: "new branch in the live tree", cmd: "git branch feature", cwd: "work", live: true, want: core.Allow},
		{name: "branch list in the live tree", cmd: "git branch --list", cwd: "work", live: true, want: core.Allow},
		{name: "branch -d in the live tree", cmd: "git branch -d feat", cwd: "work", live: true, want: core.Allow},
		{name: "update-ref help in the live tree", cmd: "git update-ref -h", cwd: "work", live: true, want: core.Allow},
	}
}

// TestRewriteGuard pins the shared-history rule: block in a shared tree, allow in an unshared or own linked one.
func TestRewriteGuard(t *testing.T) {
	work, wt := rewriteFixture(t)
	dirs := map[string]string{"work": work, "wt": wt}
	amend := "git commit --amend --no-edit"
	cases := append(rewriteRefCases(), []rewriteCase{
		{name: "live tree amend", cmd: amend, cwd: "work", live: true, want: core.Block, text: "live config tree"},
		{name: "unshared clone amend", cmd: amend, cwd: "work", want: core.Allow},
		{name: "peer rebase", cmd: "git rebase -i HEAD~1", cwd: "work", peerIn: "work", want: core.Block,
			text: "session peer-a (abcdef01, pid " + strconv.Itoa(peerPID) + ") works in it"},
		{name: "own worktree pushed amend", cmd: amend, cwd: "wt", want: core.Allow},
		{name: "own worktree leaf amend", cmd: amend, cwd: "wt", agentID: "leaf1", want: core.Allow},
		{name: "worktree with peer", cmd: amend, cwd: "wt", peerIn: "wt", want: core.Block, text: "on feat"},
		{name: "leaf outside the live tree", cmd: amend, cwd: "work", agentID: "leaf1", homeHolds: true, want: core.Allow},
		{name: "leaf in its own temp repo", cmd: amend, cwd: "work", agentID: "leaf1", want: core.Allow},
		{name: "leaf in the live tree", cmd: amend, cwd: "work", agentID: "leaf1", live: true, want: core.Block,
			text: "live config tree"},
		{name: "heredoc message names an amend", cmd: heredocMsg("git commit -m", "never git commit --amend here"),
			cwd: "work", live: true, want: core.Allow},
		{name: "heredoc message then a real amend", cmd: heredocMsg("git commit -m", "x") + " && " + amend, cwd: "work",
			live: true, want: core.Block, text: "live config tree"},
		{name: "unquoted heredoc runs a reset", cmd: "git commit -m \"$(cat <<EOF\n$(git reset --soft HEAD~1)\nEOF\n)\"",
			cwd: "work", live: true, want: core.Block, text: "live config tree"},
		{name: "live leaves in main tree", cmd: "git reset --soft HEAD~1", cwd: "work", leaves: true, live: true, want: core.Block,
			text: "1 live leaf"},
		{name: "path reset", cmd: "git reset -- one.txt", cwd: "work", peerIn: "work", want: core.Allow},
		{name: "reset HEAD", cmd: "git reset HEAD", cwd: "work", peerIn: "work", want: core.Allow},
		{name: "rebase continue", cmd: "git rebase --continue", cwd: "work", peerIn: "work", want: core.Allow},
		{name: "codex", cmd: amend, cwd: "work", peerIn: "work", agent: core.AgentCodex, want: core.Block},
		{name: "bash -c cd", cmd: `bash -c "cd ` + work + ` && git commit --amend"`, cwd: "wt", peerIn: "work",
			want: core.Block},
		{name: "timeout -s wrapper", cmd: "timeout -s KILL 5 bash -c 'git commit --amend'", cwd: "work", live: true,
			want: core.Block, text: "live config tree"},
		{name: "git missing", cmd: amend, cwd: "work", live: true, noGit: true, want: core.Allow, text: "could not check"},
		// a `sudo` ask rule must not downgrade the rewrite block to an approvable ask
		{name: "sudo clustered wrapper", cmd: "sudo -nu root git commit --amend --no-edit", cwd: "work", live: true,
			want: core.Block, text: "live config tree"},
	}...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rewriteSetup(t, c, dirs)
			cmd := strings.ReplaceAll(c.cmd, "WORK", work)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dirs[c.cwd],
				"agent_id": c.agentID, "tool_input": map[string]string{"command": cmd}})
			r := Scan(core.ParseInput(strings.NewReader(string(payload))))
			if got := r.Reason + r.Stderr; r.Decision != c.want || !strings.Contains(got, c.text) {
				t.Fatalf("decision %v, want %v containing %q:\n%s", r.Decision, c.want, c.text, got)
			}
		})
	}
}

// An explicit-branch rebase inspects that branch, not HEAD: on main at origin/main it still rewrites topic commits.
func TestRebaseExplicitBranchInspectsThatBranch(t *testing.T) {
	if got, _ := rewriteRange("rebase", []string{"origin/main", "topic"}); len(got) != 1 || got[0] != "origin/main..topic" {
		t.Fatalf("range %v, want [origin/main..topic]", got)
	}
	work, wt := rewriteFixture(t)
	tw := filepath.Join(filepath.Dir(work), "tw")
	hosttest.In(t, work, "worktree", "add", "-q", "-b", "topic", tw)
	if err := os.WriteFile(filepath.Join(tw, "t.txt"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, tw, "add", ".")
	hosttest.In(t, tw, "commit", "-qm", "t")
	rewriteSetup(t, rewriteCase{live: true}, map[string]string{"work": work, "wt": wt})
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work,
		"tool_input": map[string]string{"command": "git rebase origin/main topic"}})
	r := Scan(core.ParseInput(strings.NewReader(string(payload))))
	if got := r.Reason + r.Stderr; r.Decision != core.Block || !strings.Contains(got, "topic") {
		t.Fatalf("decision %v, want Block naming topic:\n%s", r.Decision, got)
	}
}

// A separate-value rebase flag (--empty drop) is not read as the upstream operand.
func TestRebaseEmptyFlagValueIsNotOperand(t *testing.T) {
	got, _ := rewriteRange("rebase", []string{"--empty", "drop", "main"})
	if len(got) != 1 || got[0] != "main..HEAD" {
		t.Fatalf("range %v, want [main..HEAD]", got)
	}
}
