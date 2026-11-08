package safety

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// An inert-text ask (hard-block shape only in quoted text) must not mask a real exfil block later in the command.
func TestSafetyInertAskDoesNotMaskExfilBlock(t *testing.T) {
	tierHarness(t)
	cmd := `echo "rm -rf /"; printenv > /tmp/x`
	if got, stderr := safetyScanDecision(t, bashPayload(cmd)); got != core.Block {
		t.Errorf("%q: got %v want Block (stderr %s)", cmd, got, stderr)
	}
}

// gitIn runs git in dir with a fixed identity and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// An explicit-branch rebase inspects that branch, not HEAD: on main at origin/main it still rewrites topic commits.
func TestRebaseExplicitBranchInspectsThatBranch(t *testing.T) {
	if got, _ := rewriteRange("rebase", []string{"origin/main", "topic"}); len(got) != 1 || got[0] != "origin/main..topic" {
		t.Fatalf("range %v, want [origin/main..topic]", got)
	}
	work, wt := rewriteFixture(t)
	tw := filepath.Join(filepath.Dir(work), "tw")
	gitIn(t, work, "worktree", "add", "-q", "-b", "topic", tw)
	if err := os.WriteFile(filepath.Join(tw, "t.txt"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, tw, "add", ".")
	gitIn(t, tw, "commit", "-qm", "t")
	rewriteSetup(t, rewriteCase{live: true}, map[string]string{"work": work, "wt": wt})
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work,
		"tool_input": map[string]string{"command": "git rebase origin/main topic"}})
	r := Scan(core.ParseInput(strings.NewReader(string(payload))))
	if got := r.Reason + r.Stderr; r.Decision != core.Block || !strings.Contains(got, "topic") {
		t.Fatalf("decision %v, want Block naming topic:\n%s", r.Decision, got)
	}
}

// A quoted-text hard-block Ask must keep the rewrite guard's fail-open warning when the ask pass allows.
func TestHardBlockAskKeepsRewriteWarning(t *testing.T) {
	work, wt := rewriteFixture(t)
	rewriteSetup(t, rewriteCase{live: true, noGit: true}, map[string]string{"work": work, "wt": wt})
	cmd := `echo "` + "rm -rf" + ` /"; git commit --amend --no-edit`
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": work,
		"tool_input": map[string]string{"command": cmd}})
	r := Scan(core.ParseInput(strings.NewReader(string(payload))))
	if got := r.Reason + r.Stderr; r.Decision != core.Ask || !strings.Contains(got, "could not check") {
		t.Fatalf("decision %v, want Ask carrying the could-not-check warning:\n%s", r.Decision, got)
	}
}
