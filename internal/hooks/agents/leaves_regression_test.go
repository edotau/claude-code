package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/plans"
)

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

// Regression (2026-11-10): a gemini leaf that only read docs or this gate's source handed back ungated; mentions of the
// ran-on line or the gate text in a successful tool result are not evidence.
func TestRanOnIgnoresDocAndSourceMentions(t *testing.T) {
	doc := "Envelope (quote the `ask: ran on …` line, outcome first)\nThen `Ran on:` (the `ask: ran on …` line)"
	src := "const ranOnGateMark = \"" + ranOnGateMark + "\"\nvar ranOnPattern = regexp.MustCompile(`ask: ran on |\\\\\"ran_on\\\\\":`)"
	in := leafTx(t, "gemini", "Objective: review internal/hooks/agents\n", doc, src)
	if r, blocked := leafStopBlock(in, "a1"); !blocked || r.Decision != core.Block {
		t.Fatalf("got %+v blocked=%v, want a Block: prose mentions are not a ran-on line nor a prior gate block", r, blocked)
	}
}

// The reap runs last: a reap that outruns the host timeout must find the slot already retired.
func TestStopReapsAfterRetiringSlot(t *testing.T) {
	dispatchEnv(t)
	pre(spawnPayload("u1"))
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	writeMeta(t, tx, "a1", "u1")
	slotAtReap := true
	orig := reapLeaf
	reapLeaf = func(*core.Input, leafRead) { slotAtReap = hasMarker(t, "u1") }
	t.Cleanup(func() { reapLeaf = orig })
	Stop(core.ParseInput(strings.NewReader(stopPayload("a1", tx, ""))))
	if slotAtReap {
		t.Error("reapLeaf ran while the slot marker was still held")
	}
}

// An expired dispatch note and the ran-on fail-open note both reach the user.
func TestLeafGateKeepsBothNotes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	in := gatedLeafTx(t, "Objective: x\nAgent: codex\n", startedLine())
	in.StopHookActive = true
	writeDispatch(t, leafJob, recordAt(leafJob, "running", os.Getpid(), time.Now().Add(-leafStopMaxWait-time.Minute)))
	r, blocked := leafStopBlock(in, "a1")
	if blocked || !strings.Contains(r.SystemMessage, "finished with no ran-on line") || !strings.Contains(r.SystemMessage, "limit reached") {
		t.Errorf("got %+v blocked=%v, want one message holding both the ran-on and the expiry notes", r, blocked)
	}
}
