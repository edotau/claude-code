package wthook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// claimInput is a parsed hook payload for the claims modules.
func claimInput(t *testing.T, doc map[string]any) *core.Input {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return core.ParseInput(strings.NewReader(string(b)))
}

// useClaimOwner pins the session owner pid the claims ledger files under, with no start token (old-format lines).
func useClaimOwner(t *testing.T, pid int) {
	t.Helper()
	orig, origStart := claimOwner, ownerStart
	claimOwner = func() int { return pid }
	ownerStart = func(int) (string, error) { return "", errors.New("no token in tests") }
	t.Cleanup(func() { claimOwner, ownerStart = orig, origStart })
}

// liveOwner and deadOwner are the fake session owner pids the claims tests file under; checkerOwner runs the check.
const liveOwner, deadOwner, checkerOwner = 100, 200, 300

// TestWorktreeClaimRecordsEdits: an Edit appends its path; no session writes nothing.
func TestWorktreeClaimRecordsEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	dir := paths.Real(t.TempDir())
	Claim(claimInput(t, map[string]any{"session_id": "", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	if _, err := os.Stat(worktree.ClaimsDir()); !os.IsNotExist(err) {
		t.Fatalf("no session must write no claims dir, stat err = %v", err)
	}
	Claim(claimInput(t, map[string]any{"session_id": "S1", "agent_id": "leaf1", "cwd": dir,
		"tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "S1"))
	want := []claim{{Schema: claimsSchema, Path: filepath.Join(dir, "a.txt"), Agent: "leaf1"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("claims = %+v; want %+v (ts aside)", got, want)
	}
}

// TestClaimStampsOwnerStart: each line carries the owner's start token, so a reused owner pid reads as dead.
func TestClaimStampsOwnerStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	ownerStart = func(pid int) (string, error) {
		if pid != liveOwner {
			t.Errorf("ownerStart(%d), want the owner pid %d", pid, liveOwner)
		}
		return "tok", nil
	}
	dir := paths.Real(t.TempDir())
	Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	if got := readClaims(t, claimsFile(liveOwner, "S1")); len(got) != 1 || got[0].OwnerStart != "tok" {
		t.Fatalf("claims = %+v; want one line with owner_start tok", got)
	}
}

// readClaims decodes a claims file with each ts zeroed.
func readClaims(t *testing.T, file string) []claim {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []claim
	for line := range strings.Lines(string(b)) {
		var c claim
		if err := json.Unmarshal([]byte(line), &c); err != nil || c.TS == 0 {
			t.Fatalf("bad claim line %q: %v", line, err)
		}
		c.TS = 0
		out = append(out, c)
	}
	return out
}

// TestClaimSkipsOwnerLookupWhenNothingToRecord: only an edit path or a git add/commit looks up the session owner.
func TestClaimSkipsOwnerLookupWhenNothingToRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	lookups, orig := 0, claimOwner
	claimOwner = func() int { lookups++; return liveOwner }
	t.Cleanup(func() { claimOwner = orig })
	dir := paths.Real(t.TempDir())
	for _, cmd := range []string{"ls", "git status", "grep add x"} {
		Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}}))
	}
	if lookups != 0 {
		t.Fatalf("owner looked up %d times for calls that record nothing; want 0", lookups)
	}
	Claim(claimInput(t, map[string]any{"session_id": "S1", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	got := readClaims(t, claimsFile(liveOwner, "S1"))
	if lookups != 1 || len(got) != 1 || got[0].Path != filepath.Join(dir, "a.txt") {
		t.Fatalf("edit: %d lookups, claims %+v; want one lookup and the a.txt claim", lookups, got)
	}
}

// agentInput parses doc as agent's hook would deliver it (copilot and opencode also announce themselves by shape).
func agentInput(t *testing.T, agent string, doc map[string]any) *core.Input {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return core.ParseInputFor(strings.NewReader(string(b)), agent)
}

// claimedPaths is every path in session's claims file, relative to dir.
func claimedPaths(t *testing.T, dir, session string) []string {
	t.Helper()
	var out []string
	for _, c := range readClaims(t, claimsFile(liveOwner, session)) {
		rel, _ := filepath.Rel(dir, c.Path)
		out = append(out, rel)
	}
	return out
}

// TestClaimRecordsEveryAgentsEditShape: Codex apply_patch, Copilot and OpenCode edits claim their files.
func TestClaimRecordsEveryAgentsEditShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	dir := paths.Real(t.TempDir())
	patch := "*** Begin Patch\n*** Add File: new.txt\n+x\n*** Update File: old.txt\n*** Move to: moved.txt\n@@\n-a\n+b\n" +
		"*** Delete File: gone.txt\n*** End Patch\n"
	cases := []struct {
		agent string
		doc   map[string]any
		want  []string
	}{
		{core.AgentCodex, map[string]any{"session_id": "codex1", "turn_id": "t1", "transcript_path": "", "cwd": dir,
			"hook_event_name": "PostToolUse", "model": "m", "permission_mode": "default", "tool_name": "apply_patch",
			"tool_input": map[string]any{"command": patch}, "tool_response": map[string]any{}, "tool_use_id": "u1"},
			[]string{"new.txt", "old.txt", "moved.txt", "gone.txt"}},
		{core.AgentCopilot, map[string]any{"sessionId": "cop1", "timestamp": 1, "cwd": dir, "toolCallId": "c1", "toolName": "edit",
			"toolArgs": map[string]any{"path": "sub/x.txt", "file_text": "x"}, "toolResult": map[string]any{"resultType": "success"}},
			[]string{"sub/x.txt"}},
		{core.AgentCopilot, map[string]any{"sessionId": "cop2", "timestamp": 1, "cwd": dir, "toolCallId": "c2", "toolName": "bash",
			"toolArgs": map[string]any{"command": "echo hi > out.txt"}, "toolResult": map[string]any{}},
			[]string{"out.txt"}},
		{core.AgentOpencode, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Edit", "cwd": dir, "session_id": "oc1",
			"harness_agent": "opencode", "tool_input": map[string]any{"file_path": filepath.Join(dir, "o.txt")}},
			[]string{"o.txt"}},
	}
	for _, tc := range cases {
		in := agentInput(t, tc.agent, tc.doc)
		Claim(in)
		if got := claimedPaths(t, dir, in.SessionID); !slices.Equal(got, tc.want) {
			t.Errorf("%s %v: claimed %v, want %v", tc.agent, tc.doc["toolName"], got, tc.want)
		}
	}
}

// TestClaimStampsDispatchIDForNonClaudeAgents: a dispatched Codex claim names its dispatch; Claude's do not.
func TestClaimStampsDispatchIDForNonClaudeAgents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_DISPATCH_ID", "d-77")
	useClaimOwner(t, liveOwner)
	dir := paths.Real(t.TempDir())
	edit := func(session string) map[string]any {
		return map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit", "tool_input": map[string]any{"file_path": "a.txt"}}
	}
	Claim(agentInput(t, core.AgentCodex, edit("cx")))
	Claim(agentInput(t, core.AgentClaude, edit("cl")))
	if got := readClaims(t, claimsFile(liveOwner, "cx")); got[0].Agent != "d-77" {
		t.Errorf("codex claim agent = %q, want the dispatch id d-77", got[0].Agent)
	}
	if got := readClaims(t, claimsFile(liveOwner, "cl")); got[0].Agent != "" {
		t.Errorf("claude claim agent = %q, want it unchanged (empty)", got[0].Agent)
	}
}

// TestClaimStampsDispatchFromRunningRecord: with no id in the environment, a running record of an ancestor names it.
func TestClaimStampsDispatchFromRunningRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_DISPATCH_ID", "")
	owner := os.Getpid()
	useClaimOwner(t, owner)
	orig := runningDispatches
	t.Cleanup(func() { runningDispatches = orig })
	dir := paths.Real(t.TempDir())
	for _, tc := range []struct {
		name, want string
		recs       []agent.Record
	}{
		{"job is an ancestor", "d-rec", []agent.Record{{ID: "d-rec", Status: "running", PID: os.Getppid()}}},
		{"job is unrelated", "", []agent.Record{{ID: "d-rec", Status: "running", PID: 1 << 30}}},
		{"job ended", "", []agent.Record{{ID: "d-rec", Status: "ok", PID: os.Getppid()}}},
	} {
		runningDispatches = func() ([]agent.Record, error) { return tc.recs, nil }
		session := "s-" + strings.ReplaceAll(tc.name, " ", "")
		Claim(agentInput(t, core.AgentCodex, map[string]any{"session_id": session, "cwd": dir, "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": "a.txt"}}))
		if got := readClaims(t, claimsFile(owner, session)); got[0].Agent != tc.want {
			t.Errorf("%s: stamp = %q, want %q", tc.name, got[0].Agent, tc.want)
		}
	}
}

// TestRecordDispatchScansPastNewest64: the ancestor's running record is found behind 64 newer running ones.
func TestRecordDispatchScansPastNewest64(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i := range 65 {
		r := agent.Record{ID: fmt.Sprintf("d-%03d", i), Status: "running", PID: 1 << 30, Time: time.Now()}
		if i == 0 { // oldest id: the newest-64 scan never reached it
			r.PID = os.Getpid()
		}
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if got := recordDispatch(os.Getpid()); got != "d-000" {
		t.Fatalf("recordDispatch = %q, want d-000", got)
	}
}

// TestClaimDoesNotStampFromDeadRecord: a "running" record whose job process is gone (a reused pid) names no dispatch.
func TestClaimDoesNotStampFromDeadRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_DISPATCH_ID", "")
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no true binary")
	}
	dead := cmd.Process.Pid
	useClaimOwner(t, dead)
	orig := runningDispatches
	t.Cleanup(func() { runningDispatches = orig })
	runningDispatches = func() ([]agent.Record, error) {
		return []agent.Record{{ID: "d-dead", Status: "running", PID: dead}}, nil
	}
	dir := paths.Real(t.TempDir())
	Claim(agentInput(t, core.AgentCodex, map[string]any{"session_id": "s", "cwd": dir, "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": "a.txt"}}))
	if got := readClaims(t, claimsFile(dead, "s")); len(got) == 0 || got[0].Agent != "" {
		t.Fatalf("claims = %+v, want one unstamped claim", got)
	}
}

// TestClaimSkipsFailedEdits: a patch or edit the vendor reports as failed claims nothing; an unstated outcome claims.
func TestClaimSkipsFailedEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	useClaimOwner(t, liveOwner)
	dir := paths.Real(t.TempDir())
	patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n-a\n+b\n*** End Patch\n"
	codex := func(session string, resp any) (string, map[string]any) {
		return core.AgentCodex, map[string]any{"session_id": session, "cwd": dir, "tool_name": "apply_patch",
			"tool_input": map[string]any{"command": patch}, "tool_response": resp}
	}
	copilot := func(session, result string) (string, map[string]any) {
		return core.AgentCopilot, map[string]any{"sessionId": session, "cwd": dir, "toolName": "edit",
			"toolArgs": map[string]any{"path": "a.txt"}, "toolResult": map[string]any{"resultType": result}}
	}
	type doc struct {
		agent   string
		payload map[string]any
		claims  bool
	}
	mk := func(claims bool, a string, p map[string]any) doc { return doc{a, p, claims} }
	cases := map[string]doc{}
	a, p := codex("cx-bad", "apply_patch verification failed: no such file")
	cases["cx-bad"] = mk(false, a, p)
	a, p = codex("cx-flag", map[string]any{"success": false})
	cases["cx-flag"] = mk(false, a, p)
	a, p = codex("cx-ok", "Success. Updated the following files:\nM a.txt")
	cases["cx-ok"] = mk(true, a, p)
	a, p = codex("cx-none", map[string]any{})
	cases["cx-none"] = mk(true, a, p)
	a, p = copilot("cp-bad", "failure")
	cases["cp-bad"] = mk(false, a, p)
	a, p = copilot("cp-denied", "denied")
	cases["cp-denied"] = mk(false, a, p)
	a, p = copilot("cp-ok", "success")
	cases["cp-ok"] = mk(true, a, p)
	for session, c := range cases {
		Claim(agentInput(t, c.agent, c.payload))
		_, err := os.Stat(claimsFile(liveOwner, session))
		if claimed := err == nil; claimed != c.claims {
			t.Errorf("%s: claimed = %v, want %v", session, claimed, c.claims)
		}
	}
}
