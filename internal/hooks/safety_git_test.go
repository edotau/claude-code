package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/worktree"
)

// rewriteFixture builds a bare origin, a clone with two pushed commits, and a linked worktree on a pushed branch.
func rewriteFixture(t *testing.T) (work, wt string) {
	t.Helper()
	root := canonPath(t.TempDir())
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
	return work, wt
}

// peerPID is the fake peer the swapped peerAgents seam reports.
const peerPID = 4242

type rewriteCase struct {
	name, cmd, cwd, peerIn, agentID, agent string
	live, leaves, noGit                    bool
	want                                   Decision
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
	orig := peerAgents
	peerAgents = func(top string, _ []string) ([]worktree.Proc, error) {
		if c.peerIn != "" && top == dirs[c.peerIn] {
			return []worktree.Proc{{PID: peerPID, Comm: "claude"}}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { peerAgents = orig })
	if c.leaves {
		seedLeafMarker(t)
	}
	if c.noGit {
		t.Setenv("PATH", t.TempDir())
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

// TestRewriteGuard pins the shared-history rule: block in a shared tree, allow in an unshared or own linked one.
func TestRewriteGuard(t *testing.T) {
	work, wt := rewriteFixture(t)
	dirs := map[string]string{"work": work, "wt": wt}
	amend := "git commit --amend --no-edit"
	cases := []rewriteCase{
		{name: "live tree amend", cmd: amend, cwd: "work", live: true, want: Block, text: "live config tree"},
		{name: "unshared clone amend", cmd: amend, cwd: "work", want: Allow},
		{name: "peer rebase", cmd: "git rebase -i HEAD~1", cwd: "work", peerIn: "work", want: Block,
			text: "session peer-a (abcdef01, pid " + strconv.Itoa(peerPID) + ") works in it"},
		{name: "own worktree pushed amend", cmd: amend, cwd: "wt", want: Allow},
		{name: "own worktree leaf amend", cmd: amend, cwd: "wt", agentID: "leaf1", want: Allow},
		{name: "worktree with peer", cmd: amend, cwd: "wt", peerIn: "wt", want: Block, text: "on feat"},
		{name: "leaf in main tree", cmd: amend, cwd: "work", agentID: "leaf1", want: Block, text: "leaf in the main tree"},
		{name: "live leaves in main tree", cmd: "git reset --soft HEAD~1", cwd: "work", leaves: true, want: Block,
			text: "1 live leaf"},
		{name: "path reset", cmd: "git reset -- one.txt", cwd: "work", peerIn: "work", want: Allow},
		{name: "reset HEAD", cmd: "git reset HEAD", cwd: "work", peerIn: "work", want: Allow},
		{name: "rebase continue", cmd: "git rebase --continue", cwd: "work", peerIn: "work", want: Allow},
		{name: "codex", cmd: amend, cwd: "work", peerIn: "work", agent: AgentCodex, want: Block},
		{name: "bash -c cd", cmd: `bash -c "cd ` + work + ` && git commit --amend"`, cwd: "wt", peerIn: "work",
			want: Block},
		{name: "git missing", cmd: amend, cwd: "work", live: true, noGit: true, want: Allow, text: "could not check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rewriteSetup(t, c, dirs)
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": dirs[c.cwd],
				"agent_id": c.agentID, "tool_input": map[string]string{"command": c.cmd}})
			r := safetyScan(ParseInput(strings.NewReader(string(payload))))
			if got := r.Reason + r.Stderr; r.Decision != c.want || !strings.Contains(got, c.text) {
				t.Fatalf("decision %v, want %v containing %q:\n%s", r.Decision, c.want, c.text, got)
			}
		})
	}
}
