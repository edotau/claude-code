package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/codex" // registers the codex row the label-cell pin tests resolve
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory/memorytest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/worktree"
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

// seedForeign fills n slots of the session account with markers owned by a live foreign pid (this test process).
func seedForeign(t *testing.T, n int) {
	t.Helper()
	dir := inflight.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		name := inflight.Prefix + strconv.Itoa(os.Getpid()) + "-seed" + strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(router.SessionAccount()), 0o600); err != nil {
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

// Leak 1 (2026-09-15): the claim of a background Agent is retired only by the SubagentStop resolved through the
// agent_transcript_path dir's meta.json (its PostToolUse reaches no dispatch runner: hooks wiring test).
func TestDispatchBackgroundSpawnRetiredByStop(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	runEvent(t, "PreToolUse", spawnPayload("toolu_bg"))
	if !hasMarker(t, "toolu_bg") {
		t.Fatalf("PreToolUse Agent claimed nothing: %v", markerNames(t))
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
	Stop(core.ParseInput(strings.NewReader(stop)))
	if hasMarker(t, "u1") || !hasMarker(t, "u2") {
		t.Fatalf("first stop must retire exactly u1: %v", markerNames(t))
	}
	Stop(core.ParseInput(strings.NewReader(stop)))
	Stop(core.ParseInput(strings.NewReader(stop)))
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
	if r.Decision != core.Block || r.UpdatedInput != nil {
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
	if r.Decision != core.Block || !strings.Contains(r.Reason, "ambiguous") {
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
	if r := pre(workflowPayload("wf")); r.Decision == core.Block {
		t.Fatalf("3 leaves under cap %d blocked: %s", capN, r.Reason)
	}
	for i := 1; i <= 3; i++ {
		want := inflight.Prefix + strconv.Itoa(inflight.SessionOwner()) + "-wf." + strconv.Itoa(i)
		if !slices.Contains(markerNames(t), want) {
			t.Fatalf("missing leaf marker %s in %v", want, markerNames(t))
		}
	}
	Release(core.ParseInput(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Workflow","tool_use_id":"wf"}`)))
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("workflow-inflight left %d markers", n)
	}

	seedForeign(t, capN-2)
	r := pre(workflowPayload("wf2"))
	if r.Decision != core.Block {
		t.Fatalf("3 leaves with 2 free = %+v, want Block", r)
	}
	if hasMarker(t, "wf2") || len(markerNames(t)) != capN-2 {
		t.Fatalf("a blocked batch claimed leaves: %v", markerNames(t))
	}
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

// A spaced call `agent (…)` is a leaf too; a method `x.agent(` and a name `subagent(` are not.
func TestWorkflowLeavesCountsSpacedCalls(t *testing.T) {
	script := "await agent ('a', {model:'sonnet'})\nawait agent('b', {model:'sonnet'})\nx.agent('c')\nsubagent('d')"
	in := core.ParseInput(strings.NewReader(`{"tool_name":"Workflow","tool_input":{"script":` + strconv.Quote(script) + `}}`))
	if n := workflowLeaves(in, core.Result{}); n != 2 {
		t.Fatalf("workflowLeaves = %d, want 2", n)
	}
}

// spawnPayload is an Agent PreToolUse payload with an explicit tool_use_id and pinned model.
func spawnPayload(id string) string {
	return `{"tool_name":"Agent","tool_input":{"prompt":"x","model":"sonnet"},"tool_use_id":"` + id + `"}`
}

// postFailurePayload is the PostToolUseFailure shape: a bare top-level tool_use_id, no tool_name.
func postFailurePayload(id string) string { return `{"tool_use_id":"` + id + `"}` }

// markerNames returns only agent-* entries in the inflight dir, ignoring .lock and strays.
func markerNames(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(inflight.Dir())
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), inflight.Prefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// dispatchEnv isolates the config dir and returns the cap one session may fill (min of account and total).
func dispatchEnv(t *testing.T) int {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if inflight.SessionOwner() == 0 {
		t.Skip("session owner unavailable")
	}
	lim := router.CurrentLimits()
	return min(lim.For(router.SessionAccount()), lim.Total)
}

func pre(payload string) core.Result { return Effort(core.ParseInput(strings.NewReader(payload))) }

// Cap → claim → release spans the group: spawns fill the cap, the next blocks without a marker and names the
// numbers, a failed spawn's release frees exactly one slot, and a SubagentStop resolved through meta.json frees another.
func TestDispatchCapBlockReleaseAndStop(t *testing.T) {
	capN := dispatchEnv(t)
	for i := range capN {
		if r := pre(spawnPayload(fmt.Sprintf("u%d", i))); r.Decision == core.Block {
			t.Fatalf("spawn %d under the cap blocked: %s", i, r.Reason)
		}
	}
	r := pre(spawnPayload("over"))
	if r.Decision != core.Block || !strings.Contains(r.Reason, fmt.Sprintf("%d agents already in flight", capN)) ||
		!strings.Contains(r.Reason, "run this task sequentially") {
		t.Fatalf("spawn at the cap = %+v, want a Block naming the cap and the fix", r)
	}
	if n := len(markerNames(t)); n != capN {
		t.Fatalf("a blocked dispatch must claim nothing: %d markers, want %d", n, capN)
	}
	Release(core.ParseInput(strings.NewReader(postFailurePayload("u0"))))
	if n := len(markerNames(t)); n != capN-1 {
		t.Fatalf("release of u0 left %d markers, want %d", n, capN-1)
	}
	inflightStopFreesSlot(t, capN)
}

// inflightStopFreesSlot resolves a SubagentStop through meta.json: u1's slot frees once; unknown agents free none.
func inflightStopFreesSlot(t *testing.T, capN int) {
	t.Helper()
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	writeMeta(t, tx, "a1", "u1")
	stop := `{"hook_event_name":"SubagentStop","agent_id":"a1","transcript_path":"` + tx + `"}`
	Stop(core.ParseInput(strings.NewReader(stop)))
	Stop(core.ParseInput(strings.NewReader(stop))) // a second stop is a no-op
	if n := len(markerNames(t)); n != capN-2 {
		t.Fatalf("stop of a1 left %d markers, want %d", n, capN-2)
	}
	Stop(core.ParseInput(strings.NewReader(`{"agent_id":"unknown","transcript_path":"` + tx + `"}`)))
	if n := len(markerNames(t)); n != capN-2 {
		t.Fatalf("a stop with no meta.json must retire nothing: %d markers", n)
	}
}

// A Workflow claims one slot per agent() site after the pin rewrite; the PostToolUse release frees them all.
func TestDispatchWorkflowClaimsOneSlotPerLeaf(t *testing.T) {
	capN := dispatchEnv(t)
	script := "agent('a', {model:'sonnet'});\nagent('b', {model:'sonnet'});\nawait agent('c');\n"
	payload := `{"tool_name":"Workflow","tool_use_id":"wf1","tool_input":{"script":` + fmt.Sprintf("%q", script) + `}}`
	if r := pre(payload); r.Decision == core.Block {
		t.Fatalf("workflow blocked: %s", r.Reason)
	}
	want := min(3, capN)
	if n := len(markerNames(t)); n != want {
		t.Fatalf("workflow claimed %d markers, want %d", n, want)
	}
	Release(core.ParseInput(strings.NewReader(`{"tool_name":"Workflow","tool_use_id":"wf1"}`)))
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("release left %d markers", n)
	}
}

// Non-spawn tools neither count nor claim.
func TestDispatchIgnoresOtherTools(t *testing.T) {
	dispatchEnv(t)
	if r := pre(`{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"b"}`); r.Decision == core.Block || len(markerNames(t)) != 0 {
		t.Fatalf("a Bash call claimed or blocked: %+v %v", r, markerNames(t))
	}
}

// A pin-step Block (bad workflow pin) returns as-is and claims nothing.
func TestDispatchPinBlockClaimsNothing(t *testing.T) {
	dispatchEnv(t)
	payload := `{"tool_name":"Workflow","tool_use_id":"bad","tool_input":{"script":"agent('x', {model:'gpt-4'});"}}`
	if r := pre(payload); r.Decision != core.Block {
		t.Fatalf("a bad pin must block, got %+v", r)
	}
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("a pin Block wrote %d markers", n)
	}
}

const leafJob = "20261030T101500.123-codex-abcd"

// leafEnv isolates the config dir and returns a leaf transcript whose Bash tool result printed the given lines.
func leafEnv(t *testing.T, toolResults ...string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var sb strings.Builder
	for _, r := range toolResults {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "content": r}}}})
		sb.Write(line)
		sb.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "agent-a1.jsonl")
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeDispatch(t *testing.T, id, body string) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "dispatch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func record(id, status string, pid int) string { return recordAt(id, status, pid, time.Now()) }

func recordAt(id, status string, pid int, at time.Time) string {
	return fmt.Sprintf(`{"id":%q,"time":%q,"status":%q,"pid":%d}`, id, at.Format(time.RFC3339), status, pid)
}

func leafInput(tx string) *core.Input { return &core.Input{AgentTranscriptPath: tx} }

func startedLine() string { return "job " + leafJob + " started" }

// A running dispatch blocks the stop, naming the id and the tail command, and the Stop path exits 2 with the reason.
func TestLeafStopBlocksRunningDispatch(t *testing.T) {
	tx := leafEnv(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	r, blocked := leafStopBlock(leafInput(tx), "a1")
	if !blocked || r.Decision != core.Block || !strings.Contains(r.Reason, leafJob) ||
		!strings.Contains(r.Reason, "claude-code agents tail -f "+leafJob) || !strings.Contains(r.Reason, "600000ms") {
		t.Fatalf("got %+v blocked=%v, want a Block naming %s, the tail command and the 600000ms timeout", r, blocked, leafJob)
	}
	var out, errb bytes.Buffer
	if code := core.Emit(core.AgentClaude, "SubagentStop", r, &out, &errb); code != core.ExitBlock || !strings.Contains(errb.String(), leafJob) {
		t.Fatalf("Emit = %d stderr=%q, want ExitBlock with the reason", code, errb.String())
	}
}

// Terminal status, a dead pid (even with status running), and a missing pid on a stale record all release.
func TestLeafStopReleases(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip(err)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339)
	cases := map[string]string{
		"terminal": record(leafJob, "done", os.Getpid()),
		"dead pid": record(leafJob, "running", dead.Process.Pid),
		"stale":    fmt.Sprintf(`{"id":%q,"time":%q,"status":"running"}`, leafJob, old),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			tx := leafEnv(t, startedLine())
			writeDispatch(t, leafJob, body)
			if r, blocked := leafStopBlock(leafInput(tx), "a1"); blocked || r.Decision == core.Block {
				t.Fatalf("got %+v blocked=%v, want the stop released", r, blocked)
			}
		})
	}
}

// A live dispatch blocks every stop (no block-count limit) until it is older than leafStopMaxWait, then allows with a message.
func TestLeafStopWaitLimit(t *testing.T) {
	tx := leafEnv(t, startedLine())
	writeDispatch(t, leafJob, recordAt(leafJob, "running", os.Getpid(), time.Now().Add(-time.Minute)))
	for i := range 10 {
		if _, blocked := leafStopBlock(leafInput(tx), "a1"); !blocked {
			t.Fatalf("stop %d of a 1-minute-old dispatch was allowed", i+1)
		}
	}
	writeDispatch(t, leafJob, recordAt(leafJob, "running", os.Getpid(), time.Now().Add(-leafStopMaxWait-time.Minute)))
	r, blocked := leafStopBlock(leafInput(tx), "a1")
	if blocked || r.Decision == core.Block || !strings.Contains(r.SystemMessage, leafJob) || !strings.Contains(r.SystemMessage, leafStopMaxWait.String()) {
		t.Fatalf("past the limit got %+v blocked=%v, want an allow with a message naming %s and %s", r, blocked, leafJob, leafStopMaxWait)
	}
}

// Unreadable evidence never traps a leaf: missing transcript, malformed record, missing record all allow.
func TestLeafStopFailsOpen(t *testing.T) {
	tx := leafEnv(t, startedLine())
	if _, blocked := leafStopBlock(leafInput(filepath.Join(t.TempDir(), "none.jsonl")), "a1"); blocked {
		t.Fatal("missing transcript blocked")
	}
	if _, blocked := leafStopBlock(leafInput(tx), "a1"); blocked {
		t.Fatal("missing record blocked")
	}
	writeDispatch(t, leafJob, `{not json`)
	if _, blocked := leafStopBlock(leafInput(tx), "a1"); blocked {
		t.Fatal("malformed record blocked")
	}
}

// A transcript with no dispatch allows; a non-Claude vendor is never gated.
func TestLeafStopNoDispatchAndVendor(t *testing.T) {
	tx := leafEnv(t, "ls output", "nothing here")
	if _, blocked := leafStopBlock(leafInput(tx), "a1"); blocked {
		t.Fatal("blocked with no dispatch in the transcript")
	}
	tx = leafEnv(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	t.Setenv(settings.AgentEnv, core.AgentCopilot)
	in := core.ParseInput(strings.NewReader(fmt.Sprintf(`{"agent_transcript_path":%q}`, tx)))
	if _, blocked := leafStopBlock(in, "a1"); blocked {
		t.Fatal("non-Claude vendor was gated")
	}
}

// A blocked stop keeps the leaf's inflight slot; the stop that proceeds retires it.
func TestLeafStopBlockKeepsSlot(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	sub := writeMeta(t, tx, "a1", "u1")
	agentTx := filepath.Join(sub, "agent-a1.jsonl")
	line, _ := json.Marshal(map[string]any{"type": "tool_result", "content": startedLine()})
	if err := os.WriteFile(agentTx, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	if r := pre(spawnPayload("u1")); r.Decision == core.Block {
		t.Fatalf("spawn blocked: %s", r.Reason)
	}
	stop := `{"hook_event_name":"SubagentStop","agent_id":"a1","transcript_path":"` + tx + `","agent_transcript_path":"` + agentTx + `"}`
	if r := Stop(core.ParseInput(strings.NewReader(stop))); r.Decision != core.Block {
		t.Fatalf("running dispatch: got %+v, want Block", r)
	}
	if n := len(markerNames(t)); n != 1 {
		t.Fatalf("blocked stop left %d markers, want 1 (slot kept)", n)
	}
	writeDispatch(t, leafJob, record(leafJob, "done", os.Getpid()))
	Stop(core.ParseInput(strings.NewReader(stop)))
	if n := len(markerNames(t)); n != 0 {
		t.Fatalf("proceeding stop left %d markers, want 0", n)
	}
}

// Only ids a tool_result printed count, in both the text and the --format json form; prose mentions do not.
func TestLeafJobIDs(t *testing.T) {
	const textID, jsonID, proseID = "20261030T101500.123-codex-aaaa", "20261030T101501.456-claude-bbbb", "20261030T101502.789-gemini-cccc"
	tx := filepath.Join(t.TempDir(), "agent.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I saw job ` + proseID + ` started"}]}}`,
		`{"type":"user","message":{"content":"job ` + proseID + ` started"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"job ` + textID + ` started\n"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"id\":\"` + jsonID + `\",\"status\":\"running\"}"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"job ` + textID + ` started again"}]}}`,
	}
	if err := os.WriteFile(tx, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := leafJobIDs(tx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{textID, jsonID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("leafJobIDs = %v, want %v", got, want)
	}
}

// The envelope "dispatch_id" and the `ask: dispatch` exit line count too (the old pattern missed both).
func TestLeafJobIDsEnvelopeAndExitLine(t *testing.T) {
	const envID, exitID = "20261030T101500.123-codex-aaaa", "20261030T101501.456-claude-bbbb"
	tx := filepath.Join(t.TempDir(), "agent.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"agent\":\"codex\",\"dispatch_id\":\"` + envID + `\"}"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ask: dispatch ` + exitID + ` (replay)"}]}}`,
	}
	if err := os.WriteFile(tx, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := leafJobIDs(tx)
	if err != nil || strings.Join(got, ",") != envID+","+exitID {
		t.Fatalf("leafJobIDs = %v, %v", got, err)
	}
	if _, err := leafJobIDs(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing transcript: want an error so the gate fails open")
	}
}

// gatedLeafTx writes a codex leaf transcript (the brief, then the given tool results) with its meta.json beside it.
func gatedLeafTx(t *testing.T, brief string, toolResults ...string) *core.Input {
	t.Helper()
	return leafTx(t, "codex", brief, toolResults...)
}

// leafTx is gatedLeafTx for any subagent type.
func leafTx(t *testing.T, agentType, brief string, toolResults ...string) *core.Input {
	t.Helper()
	dir := t.TempDir()
	first, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": brief}})
	body := string(first) + "\n"
	var bodySb206 strings.Builder
	for _, r := range toolResults {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "content": r}}}})
		bodySb206.WriteString(string(line) + "\n")
	}
	body += bodySb206.String()
	tx := filepath.Join(dir, "agent-a1.jsonl")
	if err := os.WriteFile(tx, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"`+agentType+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return leafInput(tx)
}

// The ran-on gate covers every leaf type that can request an agent and spares claude.
func TestRanOnGateTypes(t *testing.T) {
	for st, want := range map[string]bool{"codex": true, "copilot": true, "gemini": true, "opencode": true, "claude": false} {
		_, blocked := leafStopBlock(leafTx(t, st, "Objective: x\nAgent: codex\n", "ls output"), "a1")
		if blocked != want {
			t.Errorf("%s: blocked = %v, want %v", st, blocked, want)
		}
	}
}

// A pinned leaf type is the agent request: with no Agent: line it is still blocked, while claude is not.
func TestRanOnGatePinnedTypeNeedsNoAgentLine(t *testing.T) {
	for st, want := range map[string]bool{"codex": true, "copilot": true, "gemini": true, "opencode": true, "claude": false} {
		_, blocked := leafStopBlock(leafTx(t, st, "Objective: x\nOwns: /tmp/x\n", "ls output"), "a1")
		if blocked != want {
			t.Errorf("%s with no Agent: line: blocked = %v, want %v", st, blocked, want)
		}
	}
}

// A gated leaf whose brief requests an agent but shows no ran-on line in its tool results is blocked; prose does not count.
func TestRanOnNoRanOnBlocks(t *testing.T) {
	in := gatedLeafTx(t, "Objective: x\nAgent: codex\n", "ls output")
	r, blocked := leafStopBlock(in, "a1")
	if !blocked || r.Decision != core.Block || !strings.Contains(r.Reason, "ran on") {
		t.Fatalf("got %+v blocked=%v, want a Block naming the ran-on line", r, blocked)
	}
	var out, errb bytes.Buffer
	if code := core.Emit(core.AgentClaude, "SubagentStop", r, &out, &errb); code != core.ExitBlock {
		t.Fatalf("Emit = %d, want ExitBlock", code)
	}
}

// A tool result carrying the ask line or the json ran_on field releases the stop.
func TestRanOnRanOnReleases(t *testing.T) {
	for _, evidence := range []string{"ask: ran on codex · acct · gpt · fallback none", `{"ran_on": "ran on codex · acct · gpt · fallback none","fallback":""}`} {
		in := gatedLeafTx(t, "Objective: x\nProfile: dev\n", evidence)
		if r, blocked := leafStopBlock(in, "a1"); blocked || r.SystemMessage != "" {
			t.Fatalf("evidence %q: got %+v blocked=%v, want a silent release", evidence, r, blocked)
		}
	}
}

// The repeat stop (stop_hook_active) passes with a visible message, so a leaf is never trapped.
func TestRanOnRanOnFailOpenCap(t *testing.T) {
	in := gatedLeafTx(t, "Objective: x\nAgent: codex\n", "ls")
	in.StopHookActive = true
	r, blocked := leafStopBlock(in, "a1")
	if blocked || r.Decision == core.Block || !strings.Contains(r.SystemMessage, "a1: finished with no ran-on line") {
		t.Fatalf("got %+v blocked=%v, want an allow with the ran-on message", r, blocked)
	}
}

// An expired dispatch must not hide a younger live one started by the same leaf.
func TestLeafStopExpiredDoesNotHideLiveDispatch(t *testing.T) {
	const second = "20260101T000000.000-second"
	tx := leafEnv(t, startedLine(), "job "+second+" started")
	writeDispatch(t, leafJob, recordAt(leafJob, "running", os.Getpid(), time.Now().Add(-leafStopMaxWait-time.Minute)))
	writeDispatch(t, second, record(second, "running", os.Getpid()))
	if r, blocked := leafStopBlock(leafInput(tx), "a1"); !blocked || !strings.Contains(r.Reason, second) {
		t.Fatalf("got %+v blocked=%v, want a Block naming the live dispatch %s", r, blocked, second)
	}
}

func TestRanOnBriefContentRepresentations(t *testing.T) {
	brief := "Objective: inspect only\nAgent: codex\n"
	for _, tc := range []struct {
		name    string
		content any
		spaced  bool
	}{
		{"string", brief, false},
		{"text parts", []map[string]any{{"type": "text", "text": brief}}, false},
		{"spaced string", brief, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gatedLeafTx(t, brief)
			line, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": tc.content}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.spaced {
				line = bytes.ReplaceAll(line, []byte(`":"`), []byte(`": "`))
			}
			if err := os.WriteFile(in.AgentTranscriptPath, append(line, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			r, blocked := leafStopBlock(in, "a1")
			if !blocked || r.Decision != core.Block || !strings.Contains(r.Reason, "ran on") {
				t.Fatalf("got %+v blocked=%v, want missing ran-on evidence to block", r, blocked)
			}
		})
	}
}

// A gated leaf hand-back with no ran-on line is denied; a delivered hand-back lets the later stop pass.
func TestHandbackGatesRanOn(t *testing.T) {
	src := gatedLeafTx(t, "Objective: x\nProfile: dev\n", "ls output")
	parent := filepath.Join(t.TempDir(), "session.jsonl")
	sub := inflight.SubagentDir(parent)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-a1.jsonl", "agent-a1.meta.json"} {
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(src.AgentTranscriptPath), name))
		if err := os.WriteFile(filepath.Join(sub, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"SubagentHandback","agent_id":"a1","transcript_path":"` + parent + `"}`
	if r := Handback(core.ParseInput(strings.NewReader(payload))); r.Decision != core.Block || !strings.Contains(r.Reason, ranOnGateMark) {
		t.Fatalf("first hand-back must be denied with the gate reason, got %+v", r)
	}
	in := gatedLeafTx(t, "Objective: x\nProfile: dev\n", `{"success":true,"message":"`+handedBackMark+`."}`)
	if r, blocked := leafStopBlock(in, "a1"); blocked {
		t.Errorf("after a delivered hand-back the gate must pass, got %+v", r)
	}
}

// appendLines appends raw transcript lines to the leaf transcript at path.
func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// gateBlockLines is one prior ran-on block as Claude Code records it: a denied hand-back, or SubagentStop feedback.
func gateBlockLines(reason string) []string {
	denied, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result",
		"is_error": true, "content": "PreToolUse:SubagentHandback hook error: [$HOME/.claude/bin/claude-code hook PreToolUse]: " + reason}}}})
	feedback, _ := json.Marshal(map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"content": "SubagentStop hook feedback:\n" + reason}})
	return []string{string(denied), string(feedback)}
}

// A gemini leaf with no ran-on line is blocked on its 1st, 2nd and 3rd stop and passes the 4th with the visible note.
func TestRanOnGateBlocksThreeTimes(t *testing.T) {
	for form := range 2 {
		in := leafTx(t, "gemini", "Objective: review internal/gemini, read-only\n", "ls output")
		for n := 1; n <= 3; n++ { // literal: the 4th stop passes
			r, blocked := leafStopBlock(in, "a1")
			if !blocked || !strings.Contains(r.Reason, "--read-only") {
				t.Fatalf("form %d stop %d: got %+v blocked=%v, want a Block naming --read-only", form, n, r, blocked)
			}
			appendLines(t, in.AgentTranscriptPath, gateBlockLines(r.Reason)[form])
			in.StopHookActive = form == 1
		}
		r, blocked := leafStopBlock(in, "a1")
		if blocked || !strings.Contains(r.SystemMessage, "a1: finished with no ran-on line") {
			t.Fatalf("form %d stop %d: got %+v blocked=%v, want a pass with the ran-on note", form, 4, r, blocked)
		}
	}
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

// A Workflow call through Effort is linted by quality.WorkflowResult: an unpinned leaf gets a model.
func TestDispatchEffortLintsWorkflowViaQuality(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res := pre(`{"tool_name":"Workflow","tool_input":{"script":"agent('x', { label: 'l' });\n"}}`)
	if got, _ := res.UpdatedInput["script"].(string); !strings.Contains(got, "model: 'sonnet'") {
		t.Errorf("updatedInput.script = %q, want model: 'sonnet' filled", got)
	}
}

// TestMain stubs the live-process scans and detached spawns so no test reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &hosttest.MainTB{}
	memorytest.StubSpawn(tb, true)
	wthook.PeerAgents = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}

// runEvent runs the dispatch runner a payload's event is declared on.
func runEvent(t *testing.T, event, payload string) string {
	t.Helper()
	runners := map[string]core.Runner{settings.EventPreToolUse: Effort, settings.EventPostToolUseFailure: Release,
		settings.EventSubagentStop: Stop}
	in := core.ParseInput(strings.NewReader(payload))
	var out, errb bytes.Buffer
	core.Emit(in.Agent(), event, runners[event](in), &out, &errb)
	return out.String()
}
