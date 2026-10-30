package hooks

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/inflight"
)

// writeMeta lays down <transcript dir>/<session>/subagents/agent-<agent>.meta.json mapping agent → toolUseId.
func writeMeta(t *testing.T, tx, agent, toolUseID string) string {
	t.Helper()
	sub := inflight.SubagentDir(tx)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-"+agent+".meta.json"), []byte(`{"toolUseId":"`+toolUseID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return sub
}

func stopPayload(agent, tx, agentTx string) string {
	return `{"hook_event_name":"SubagentStop","agent_id":"` + agent + `","transcript_path":"` + tx + `","agent_transcript_path":"` + agentTx + `"}`
}

// runEvent drives the full dispatcher for one event payload.
func runEvent(t *testing.T, event, payload string) string {
	t.Helper()
	var out, errb bytes.Buffer
	RunEvent(event, false, strings.NewReader(payload), &out, &errb)
	return out.String()
}

// seedForeign fills n slots of the session account with markers owned by a live foreign pid (this test process).
func seedForeign(t *testing.T, n int) {
	t.Helper()
	dir := inflight.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		name := inflight.Prefix + strconv.Itoa(os.Getpid()) + "-seed" + strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(inflight.SessionAccount()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func hasMarker(t *testing.T, id string) bool {
	t.Helper()
	want := inflight.Prefix + strconv.Itoa(inflight.SessionOwner()) + "-" + id
	for _, n := range markerNames(t) {
		if n == want || strings.HasPrefix(n, want+".") {
			return true
		}
	}
	return false
}

// Leak 1 (2026-09-15): a background Agent's PostToolUse returns at once with a task id and must NOT retire the
// claim; only the SubagentStop resolved through the agent_transcript_path dir's meta.json does.
func TestDispatchBackgroundSpawnSurvivesPostToolUseUntilStop(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	runEvent(t, "PreToolUse", spawnPayload("toolu_bg"))
	if !hasMarker(t, "toolu_bg") {
		t.Fatalf("PreToolUse Agent claimed nothing: %v", markerNames(t))
	}
	runEvent(t, "PostToolUse", `{"hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_bg","transcript_path":"`+tx+
		`","tool_response":{"status":"async_launched","agentId":"abg"}}`)
	if !hasMarker(t, "toolu_bg") {
		t.Fatalf("a background Agent's PostToolUse retired the claim too early: %v", markerNames(t))
	}
	sub := writeMeta(t, tx, "abg", "toolu_bg")
	runEvent(t, "SubagentStop", stopPayload("abg", "", filepath.Join(sub, "agent-abg.jsonl")))
	if hasMarker(t, "toolu_bg") {
		t.Fatalf("SubagentStop via agent_transcript_path meta.json left the claim: %v", markerNames(t))
	}
}

// Leak 2 (2026-10-18): SubagentHandback ends with only ONE stop; the first retires the slot, a repeat retires
// nothing else, so a sibling's marker survives.
func TestDispatchRepeatStopRetiresNothingElse(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	pre(spawnPayload("u1"))
	pre(spawnPayload("u2"))
	writeMeta(t, tx, "a1", "u1")
	stop := stopPayload("a1", tx, "")
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
	if hasMarker(t, "u1") || !hasMarker(t, "u2") {
		t.Fatalf("first stop must retire exactly u1: %v", markerNames(t))
	}
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
	if !hasMarker(t, "u2") || len(markerNames(t)) != 1 {
		t.Fatalf("a repeat stop retired a sibling's marker: %v", markerNames(t))
	}
}

// Order cap → pin → claim: at the cap an Agent spawn blocks with no marker and no updatedInput.
func TestDispatchAtCapSpawnBlocksBeforePinAndClaim(t *testing.T) {
	capN := dispatchEnv(t)
	seedForeign(t, capN)
	// No model: the pin step would rewrite the input if it ran.
	r := pre(`{"tool_name":"Agent","tool_use_id":"over","tool_input":{"prompt":"fix a typo"}}`)
	if r.Decision != Block || r.UpdatedInput != nil {
		t.Fatalf("spawn at the cap = %+v, want a Block with no updatedInput", r)
	}
	if hasMarker(t, "over") || len(markerNames(t)) != capN {
		t.Fatalf("a blocked spawn wrote a marker: %v", markerNames(t))
	}
}

// A Workflow the lint blocks (ambiguous agent() options) claims nothing even with every slot free.
func TestDispatchLintBlockedWorkflowClaimsNothing(t *testing.T) {
	dispatchEnv(t)
	r := pre(`{"tool_name":"Workflow","tool_use_id":"amb","tool_input":{"script":"const o = {}; await agent('x', o);"}}`)
	if r.Decision != Block || !strings.Contains(r.Reason, "ambiguous") {
		t.Fatalf("ambiguous agent() options = %+v, want a lint Block", r)
	}
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("a lint-blocked Workflow claimed %d markers", n)
	}
}

const threeLeaves = "agent('a', {model:'sonnet'});\nagent('b', {model:'sonnet'});\nagent('c', {model:'sonnet'});\n"

func workflowPayload(id string) string {
	return `{"tool_name":"Workflow","tool_use_id":"` + id + `","tool_input":{"script":` + fmt.Sprintf("%q", threeLeaves) + `}}`
}

// A Workflow claims <id>.1..<id>.3 in one pass; with only two slots free it blocks and claims nothing; the
// PostToolUse release frees all leaves.
func TestDispatchWorkflowBatchIsAllOrNothing(t *testing.T) {
	capN := dispatchEnv(t)
	if capN < 3 {
		t.Skipf("cap %d too small for a 3-leaf batch", capN)
	}
	if r := pre(workflowPayload("wf")); r.Decision == Block {
		t.Fatalf("3 leaves under cap %d blocked: %s", capN, r.Reason)
	}
	for i := 1; i <= 3; i++ {
		want := inflight.Prefix + strconv.Itoa(inflight.SessionOwner()) + "-wf." + strconv.Itoa(i)
		if !contains(markerNames(t), want) {
			t.Fatalf("missing leaf marker %s in %v", want, markerNames(t))
		}
	}
	releaseSpawn(ParseInput(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Workflow","tool_use_id":"wf"}`)))
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("workflow-inflight left %d markers", n)
	}

	seedForeign(t, capN-2)
	r := pre(workflowPayload("wf2"))
	if r.Decision != Block {
		t.Fatalf("3 leaves with 2 free = %+v, want Block", r)
	}
	if hasMarker(t, "wf2") || len(markerNames(t)) != capN-2 {
		t.Fatalf("a blocked batch claimed leaves: %v", markerNames(t))
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// agent-inflight-post (PostToolUseFailure) releases an Agent's exact id and leaves others.
func TestDispatchFailureReleasesExactAgentID(t *testing.T) {
	dispatchEnv(t)
	pre(spawnPayload("keep"))
	pre(spawnPayload("gone"))
	runEvent(t, "PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","tool_name":"Agent","tool_use_id":"gone"}`)
	if hasMarker(t, "gone") || !hasMarker(t, "keep") {
		t.Fatalf("failure release not exact: %v", markerNames(t))
	}
}

func labelOf(t *testing.T, subagentType, prompt, desc string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":%q}}`,
		subagentType, prompt, desc)
	r := agentEffortResult(ParseInput(strings.NewReader(payload)))
	if r.UpdatedInput == nil {
		return desc
	}
	d, _ := r.UpdatedInput["description"].(string)
	return d
}

func TestDispatchAgentLabels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prompt := "Agent: gemini → codex\nreview the thing"
	got := labelOf(t, "code-worker", prompt, "review thing")
	if got != "gemini → codex: review thing" {
		t.Fatalf("label = %q, want %q", got, "gemini → codex: review thing")
	}
	// Running twice (the labelled description fed back in) does not double-prefix.
	if again := labelOf(t, "code-worker", prompt, got); again != got {
		t.Errorf("second run = %q, want unchanged %q", again, got)
	}
	if got := labelOf(t, "Explore", prompt, "review thing"); got != "review thing" {
		t.Errorf("Explore type relabelled: %q", got)
	}
	cases := []struct{ st, agent, want string }{
		{"code-worker", "none", "task"},
		{"claude", "copilot", "copilot: task"},
		{"copilot", "claude", "claude: task"},
		{"coworker", "codex", "codex: task"},
		{"code-worker", "claude", "claude: task"},
		{"claude", "claude (in-context)", "task"},
		{"claude", "claude", "task"},
		{"codex", "codex → gemini", "gemini: task"},
	}
	for _, c := range cases {
		if got := labelOf(t, c.st, "do it\nAgent: "+c.agent, "task"); got != c.want {
			t.Errorf("%s + Agent: %s = %q, want %q", c.st, c.agent, got, c.want)
		}
	}
}

// A spaced call `agent (…)` is a leaf too; a method `x.agent(` and a name `subagent(` are not.
func TestWorkflowLeavesCountsSpacedCalls(t *testing.T) {
	script := "await agent ('a', {model:'sonnet'})\nawait agent('b', {model:'sonnet'})\nx.agent('c')\nsubagent('d')"
	in := ParseInput(strings.NewReader(`{"tool_name":"Workflow","tool_input":{"script":` + strconv.Quote(script) + `}}`))
	if n := workflowLeaves(in, Result{}); n != 2 {
		t.Fatalf("workflowLeaves = %d, want 2", n)
	}
}
