package hooks

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
)

func allowInput(t *testing.T, tool, agentID, cwd string, in map[string]any) *core.Input {
	t.Helper()
	payload := map[string]any{"tool_name": tool, "session_id": "S", "cwd": cwd, "tool_input": in}
	if agentID != "" {
		payload["agent_id"] = agentID
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return core.ParseInput(bytes.NewReader(b))
}

// A leaf's Bash block from docs-placement or worktree-claim-check is allowed with a note; the module still blocks.
func TestLeafBashAllowLiftsPlacementAndClaimCheck(t *testing.T) {
	t.Setenv(proc.DispatchIDEnv, "")
	repo, _ := filepath.EvalSymlinks(hosttest.Repo(t))
	t.Setenv("CLAUDE_CONFIG_DIR", repo) // the repo is the live config tree, where a leaf's claim check blocks
	cases := []struct {
		module, cmd string
		raw         core.Runner
	}{
		{"docs-placement", "echo hi > NOTES.md", docs.Placement},
		{"worktree-claim-check", "claude-code worktree commit -m x", wthook.ClaimCheck},
	}
	for _, c := range cases {
		in := map[string]any{"command": c.cmd}
		if r := c.raw(allowInput(t, "Bash", "a1", repo, in)); r.Decision != core.Block {
			t.Fatalf("%s raw for a leaf: %q gave %v, want a block to lift", c.module, c.cmd, r.Decision)
		}
		r := native[c.module](allowInput(t, "Bash", "a1", repo, in))
		if r.Decision != core.Allow || r.Verdict != core.VerdictPassed || !strings.Contains(r.Context, "allowed for an agent") {
			t.Errorf("%s as a leaf: decision %v verdict %q context %q, want allow with a note", c.module, r.Decision, r.Verdict, r.Context)
		}
	}
}

// The lift is Bash-only and never lifts a Floor block (the Plan agent's plans-home confinement).
func TestLeafBashAllowSkipsEditsAndFloor(t *testing.T) {
	t.Setenv(proc.DispatchIDEnv, "")
	block := func(floor bool) core.Runner {
		return func(*core.Input) core.Result {
			return core.Result{Decision: core.Block, Reason: "no", Rule: "r", Floor: floor}
		}
	}
	cmd := map[string]any{"command": "x"}
	bash := allowInput(t, "Bash", "a1", "", cmd)
	main := allowInput(t, "Bash", "", "", cmd)
	edit := allowInput(t, "Edit", "a1", "", map[string]any{"file_path": "/tmp/x.md"})
	if r := leafBashAllow(block(false), bash); r.Decision != core.Allow || !strings.Contains(r.Context, "r\nno") {
		t.Errorf("leaf Bash block: %+v, want allow with the rule and reason in the note", r)
	}
	if r := leafBashAllow(block(true), bash); r.Decision != core.Block {
		t.Errorf("leaf Bash floor block lifted: %+v", r)
	}
	if r := leafBashAllow(block(false), edit); r.Decision != core.Block {
		t.Errorf("leaf Edit block lifted: %+v", r)
	}
	if r := leafBashAllow(block(false), main); r.Decision != core.Block {
		t.Errorf("main Bash block lifted: %+v", r)
	}
	t.Setenv(proc.DispatchIDEnv, "20261124T000000.000-codex") // a dispatched agent has no agent_id
	if r := leafBashAllow(block(false), main); r.Decision != core.Allow {
		t.Errorf("dispatched Bash block not lifted: %+v", r)
	}
}
