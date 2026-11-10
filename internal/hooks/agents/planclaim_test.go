package agents

import (
	"encoding/json"
	"github.com/edotau/claude-code/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/plans"
)

// The dispatch spawn hooks claim and release the active plan task through docs.ClaimPlanTask/ClearPlanClaim.
// planRepo makes a git repo with one dated plan holding open owned tasks 1.2 and 1.3; returns repo and plan path.
func planRepo(t *testing.T) (repo, plan string) {
	t.Helper()
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	dir := plans.Home(memory.GitToplevel(repo))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plan = filepath.Join(dir, "2026-10-29-x.md")
	body := "# P\n\n- [ ] 1.2 wire the claim call point owns: a.go\n- [ ] 1.3 delete dead recall helper owns: b.go\n"
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, plan
}

func agentPayload(repo, id, prompt, desc string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": id, "cwd": repo,
		"tool_input": map[string]any{"prompt": prompt, "description": desc, "model": "sonnet"}})
	return string(b)
}

// A Task: brief line claims that plan task on the Agent spawn and the agent's SubagentStop clears it.
func TestPlanClaimOnSpawnClearedOnStop(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	pre(agentPayload(repo, "toolu_c1", "Objective: x\nTask: 1.2\nOwns: a.go", ""))
	data, _ := os.ReadFile(plan)
	if !regexp.MustCompile(`1\.2 wire.*<!-- working: Agent toolu_c1 · worktree main · since \d\d:\d\d -->`).Match(data) || strings.Count(string(data), "working:") != 1 {
		t.Fatalf("claim not written on 1.2 only:\n%s", data)
	}
	writeMeta(t, tx, "ac1", "toolu_c1")
	stop := `{"hook_event_name":"SubagentStop","agent_id":"ac1","cwd":"` + repo + `","transcript_path":"` + tx + `"}`
	Stop(core.ParseInput(strings.NewReader(stop)))
	if data, _ = os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived SubagentStop:\n%s", data)
	}
}

// No Task: line and no confident description match claims nothing; a unique title match claims.
func TestPlanClaimNeedsConfidentMatch(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	pre(agentPayload(repo, "toolu_n1", "no task line", "do things"))
	pre(agentPayload(repo, "toolu_n2", "Task: 9.9", ""))
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("unconfident dispatch claimed a task:\n%s", data)
	}
	pre(agentPayload(repo, "toolu_n3", "no task line", "Delete dead recall helper"))
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "1.3 delete dead recall helper owns: b.go <!-- working: Agent toolu_n3") {
		t.Fatalf("unique description match not claimed:\n%s", data)
	}
}

// A spawn that fails never reaches SubagentStop: PostToolUseFailure must release its plan claim too.
func TestPlanClaimClearedOnSpawnFailure(t *testing.T) {
	dispatchEnv(t)
	repo, plan := planRepo(t)
	pre(agentPayload(repo, "toolu_f1", "Task: 1.2", ""))
	if data, _ := os.ReadFile(plan); !strings.Contains(string(data), "working:") {
		t.Fatalf("no claim:\n%s", data)
	}
	runEvent(t, "PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","tool_name":"Agent","tool_use_id":"toolu_f1","cwd":"`+repo+`"}`)
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived a failed spawn:\n%s", data)
	}
}
