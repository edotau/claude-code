package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/codex" // registers the codex row the label-cell pin tests resolve
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
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
		if err := os.WriteFile(filepath.Join(dir, name), []byte(router.SessionAccountContext(context.Background())), 0o600); err != nil {
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

// Leak 1 (2026-09-15): a background Agent's claim is retired only by SubagentStop resolved via meta.json.
func TestDispatchBackgroundSpawnRetiredByStop(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	runLeafEvent(t, "PreToolUse", spawnPayload("toolu_bg"))
	if !hasMarker(t, "toolu_bg") {
		t.Fatalf("PreToolUse Agent claimed nothing: %v", markerNames(t))
	}
	sub := writeMeta(t, tx, "abg", "toolu_bg")
	runLeafEvent(t, "SubagentStop", stopPayload("abg", "", filepath.Join(sub, "agent-abg.jsonl")))
	if hasMarker(t, "toolu_bg") {
		t.Fatalf("SubagentStop via agent_transcript_path meta.json left the claim: %v", markerNames(t))
	}
}

// Leak 2 (2026-10-18): SubagentHandback ends with ONE stop; a repeat retires nothing, so a sibling's marker survives.
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

// A Workflow claims <id>.1..<id>.3 in one pass; with two slots free it blocks and claims nothing.
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
	runLeafEvent(t, "PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","tool_name":"Agent","tool_use_id":"gone"}`)
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
	return min(lim.For(router.SessionAccountContext(context.Background())), lim.Total)
}

func pre(payload string) core.Result { return Effort(core.ParseInput(strings.NewReader(payload))) }

// Cap → claim → release: spawns fill the cap, the next blocks naming the numbers, each release frees one slot.
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

// leafEnv isolates the config dir and returns a leaf transcript whose Bash ask calls printed the given lines.
func leafEnv(t *testing.T, toolResults ...string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var sb strings.Builder
	for i, r := range toolResults {
		sb.WriteString(askCallLine("toolu_" + strconv.Itoa(i)))
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_" + strconv.Itoa(i), "content": r}}}})
		sb.Write(line)
		sb.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "agent-a1.jsonl")
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// askCallLine is an assistant line whose Bash call id runs an ask: only its results can name the leaf's jobs.
func askCallLine(id string) string {
	line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": `claude-code agents ask --agent codex "x"`}}}}})
	return string(line) + "\n"
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

// A running dispatch blocks the stop, naming the id and the result -f command, and exits 2.
func TestLeafStopBlocksRunningDispatch(t *testing.T) {
	tx := leafEnv(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	r, blocked := leafStopBlock(leafInput(tx), "a1")
	if !blocked || r.Decision != core.Block || !strings.Contains(r.Reason, leafJob) ||
		!strings.Contains(r.Reason, "claude-code agents result -f "+leafJob) || !strings.Contains(r.Reason, "600000ms") {
		t.Fatalf("got %+v blocked=%v, want a Block naming %s, the result -f command and the 600000ms timeout", r, blocked, leafJob)
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

// A live dispatch blocks every stop until it is older than leafStopMaxWait, then allows with a message.
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
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "u", "content": startedLine()}}}})
	if err := os.WriteFile(agentTx, append([]byte(askCallLine("u")), append(line, '\n')...), 0o600); err != nil {
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
		strings.TrimSpace(askCallLine("t")),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"job ` + textID + ` started\n"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"{\"id\":\"` + jsonID + `\",\"status\":\"running\"}"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"job ` + textID + ` started again"}]}}`,
	}
	if err := os.WriteFile(tx, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lr := readLeaf(tx, leafWant{ids: true})
	got := lr.ids
	if lr.openErr != nil {
		t.Fatal(lr.openErr)
	}
	if want := []string{textID, jsonID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("readLeaf ids = %v, want %v", got, want)
	}
}

// The envelope "dispatch_id" and the `ask: dispatch` exit line count too (the old pattern missed both).
func TestLeafJobIDsEnvelopeAndExitLine(t *testing.T) {
	const envID, exitID = "20261030T101500.123-codex-aaaa", "20261030T101501.456-claude-bbbb"
	tx := filepath.Join(t.TempDir(), "agent.jsonl")
	lines := []string{
		strings.TrimSpace(askCallLine("t")),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"{\"agent\":\"codex\",\"dispatch_id\":\"` + envID + `\"}"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"ask: dispatch ` + exitID + ` (replay)"}]}}`,
	}
	if err := os.WriteFile(tx, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lr := readLeaf(tx, leafWant{ids: true})
	if got := lr.ids; lr.openErr != nil || strings.Join(got, ",") != envID+","+exitID {
		t.Fatalf("leafJobIDs = %v, %v", got, lr.openErr)
	}
	if lr := readLeaf(filepath.Join(t.TempDir(), "missing"), leafWant{ids: true}); lr.openErr == nil {
		t.Fatal("missing transcript: want an error so the gate fails open")
	}
}

// gatedLeafTx exercises the legacy/fallback external-type gate; current workflow fixtures use claude with Run:.
func gatedLeafTx(t *testing.T, brief string, toolResults ...string) *core.Input {
	t.Helper()
	return leafTx(t, "codex", brief, toolResults...)
}

// leafTx is gatedLeafTx for any subagent type.
func leafTx(t *testing.T, agentType, brief string, toolResults ...string) *core.Input {
	t.Helper()
	command := `claude-code agents ask --agent codex "x"`
	if len(toolResults) == 1 && toolResults[0] == "ls output" {
		command = "ls"
	}
	return leafTxCmd(t, agentType, command, brief, toolResults...)
}

// leafTxCmd is leafTx where every result answers the given Bash command.
func leafTxCmd(t *testing.T, agentType, command, brief string, toolResults ...string) *core.Input {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // fresh dispatch records and hook-run markers per leaf
	dir := t.TempDir()
	first, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": brief}})
	body := string(first) + "\n"
	var bodySb206 strings.Builder
	for i, r := range toolResults { // each result answers a Bash call that ran an ask, the only results the gate reads
		id := "toolu_" + strconv.Itoa(i)
		call, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}})
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": id, "content": r}}}})
		bodySb206.Write(call)
		bodySb206.WriteByte('\n')
		bodySb206.Write(line)
		bodySb206.WriteByte('\n')
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

// `agents result -f` carries the ran-on receipt the way `tail -f` did; the removed `agents tail` earns none.
func TestRanOnReceiptFromResultFollow(t *testing.T) {
	const receipt = "ask: ran on codex · gpt-5 · profile x\nresult"
	for command, wantNote := range map[string]bool{
		"claude-code agents result -f job1": false, "claude-code agents result job1": false, "rtk claude-code codex ask x": false, "claude-code agents tail -f job1": true,
	} {
		in := leafTxCmd(t, "codex", command, "Objective: x\nAgent: codex\n", receipt+"\nask: dispatch "+leafJob)
		writeDispatch(t, leafJob, record(leafJob, "ok", 0))
		if r, blocked := leafStopBlock(in, "a1"); blocked || strings.Contains(r.SystemMessage, relayNote) != wantNote {
			t.Errorf("%q: got %+v blocked=%v, want the relay note %v", command, r, blocked, wantNote)
		}
	}
}

// relayNote is the note a gated leaf that did the work itself passes with.
const relayNote = "NOTE: ran on the relay model"

// Pi and legacy/fallback external types activate the ran-on gate; bare claude is exempt.
func TestRanOnGateTypes(t *testing.T) {
	for st, want := range map[string]bool{"pi": true, "codex": true, "copilot": true, "gemini": true, "opencode": true, "claude": false} {
		for _, brief := range []string{"Objective: x\nAgent: codex\n", "Objective: x\nOwns: /tmp/x\n"} {
			r, blocked := leafStopBlock(leafTx(t, st, brief, "ls output"), "a1")
			if blocked || strings.Contains(r.SystemMessage, relayNote) != want {
				t.Errorf("%s %q: got %+v blocked=%v, want the relay note %v and no block", st, brief, r, blocked, want)
			}
		}
	}
}

// A tool result carrying the ask line or the json ran_on field releases the stop.
func TestRanOnRanOnReleases(t *testing.T) {
	for _, evidence := range []string{"ask: ran on codex · acct · gpt · fallback none", `{"ran_on": "ran on codex · acct · gpt · fallback none","fallback":""}`} {
		in := gatedLeafTx(t, "Objective: x\nProfile: dev\n", evidence+"\nask: dispatch "+leafJob)
		writeDispatch(t, leafJob, record(leafJob, "ok", 0))
		if r, blocked := leafStopBlock(in, "a1"); blocked || r.SystemMessage != "" {
			t.Fatalf("evidence %q: got %+v blocked=%v, want a silent release", evidence, r, blocked)
		}
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
	brief := "Run: claude-code agents ask --agent codex 'inspect only'\nAgent: codex\n"
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
			spawned := stubSpawn(t)
			if _, blocked := leafStopBlock(in, "a1"); !blocked || len(*spawned) != 1 {
				t.Fatalf("blocked=%v spawns %v, want the brief's Run: line read in every content shape", blocked, *spawned)
			}
		})
	}
}

// A gated leaf hand-back with no ran-on line passes with the relay note; a delivered hand-back lets the stop pass.
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
	payload := `{"hook_event_name":"PreToolUse","tool_name":"SubagentHandback","agent_id":"a1","transcript_path":"` + parent +
		`","tool_input":{"message":"my findings"}}`
	r := Handback(core.ParseInput(strings.NewReader(payload)))
	if msg, _ := r.UpdatedInput["message"].(string); r.Decision == core.Block || !strings.HasPrefix(msg, relayNote) || !strings.HasSuffix(msg, "my findings") {
		t.Fatalf("hand-back = %+v, want it through with the relay note on top of the report", r)
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

// stubSpawn records each detached launch of a brief's Run: line instead of starting it.
func stubSpawn(t *testing.T) *[][]string {
	t.Helper()
	var got [][]string
	orig := spawnBriefLine
	t.Cleanup(func() { spawnBriefLine = orig })
	spawnBriefLine = func(words []string, _, _ string, _ *os.File) error {
		got = append(got, words)
		return nil
	}
	return &got
}

// A relay whose only edit is a memory report did not do the work: its never-run Run: line is run or held.
func TestRanOnMemoryReportEditIsNotTheWork(t *testing.T) {
	spawned := stubSpawn(t)
	in := leafTx(t, "claude", "Run: claude-code agents ask --agent gemini 'research'\n", "ls output")
	report := paths.MemorySub("reports", "2026-11-16-x.md")
	edit, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": "e1", "name": "Write", "input": map[string]any{"file_path": report}}}}})
	appendLines(t, in.AgentTranscriptPath, string(edit))
	in = handbackOf(in, "report written")
	_, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	if !blocked && len(*spawned) == 0 {
		t.Fatalf("blocked=%v spawns %v, want the Run: line run or held despite the memory report edit", blocked, *spawned)
	}
}

// A relay that edited files passes with the note on top and the Run: line is never run by the hook.
func TestRanOnRelayDidTheWork(t *testing.T) {
	spawned := stubSpawn(t)
	in := leafTx(t, "claude", "Run: claude-code agents ask --agent codex 'fix it'\n", "ls output")
	edit, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": "e1", "name": "Edit", "input": map[string]any{}}}}})
	appendLines(t, in.AgentTranscriptPath, string(edit), txResult("e1", "edited"))
	in = handbackOf(in, "fixed it")
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	msg, _ := r.UpdatedInput["message"].(string)
	if blocked || len(*spawned) != 0 || !strings.HasPrefix(msg, relayNote) || !strings.Contains(msg, "not codex") || !strings.HasSuffix(msg, "fixed it") {
		t.Fatalf("got %+v blocked=%v spawns %v, want the hand-back through with the relay note, no launch", r, blocked, *spawned)
	}
}

// Every held hand-back saves the report it carried to the reports home and names the file.
func TestRanOnHeldReportPreserved(t *testing.T) {
	stubSpawn(t)
	in := leafTx(t, "claude", "Run: claude-code agents ask --agent codex 'x'\n", "ls output")
	in = handbackOf(in, "the full report")
	in.CWD = t.TempDir()
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	_, path, ok := strings.Cut(r.Reason, "is saved at ")
	path = strings.TrimSuffix(path, ".")
	got, err := os.ReadFile(path)
	if !blocked || !ok || err != nil || string(got) != "the full report" {
		t.Fatalf("held = %+v blocked=%v; saved %q (%v), want the report preserved and named", r, blocked, got, err)
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

// A Workflow call through Effort is linted by workflows.WorkflowResult: an unpinned leaf gets a model.
func TestDispatchEffortLintsWorkflowViaQuality(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	res := pre(`{"tool_name":"Workflow","tool_input":{"script":"agent('x', { label: 'l' });\n"}}`)
	if got, _ := res.UpdatedInput["script"].(string); !strings.Contains(got, "model: 'sonnet'") {
		t.Errorf("updatedInput.script = %q, want model: 'sonnet' filled", got)
	}
}

// runLeafEvent runs the dispatch runner a payload's event is declared on.
func runLeafEvent(t *testing.T, event, payload string) string {
	t.Helper()
	runners := map[string]core.Runner{settings.EventPreToolUse: Effort, settings.EventPostToolUseFailure: Release,
		settings.EventSubagentStop: Stop}
	in := core.ParseInput(strings.NewReader(payload))
	var out, errb bytes.Buffer
	core.Emit(in.Agent(), event, runners[event](in), &out, &errb)
	return out.String()
}

// Regression (2026-11-13): a long ask result is persisted with a 2 KB preview, so the gate must read the saved output.
func TestRanOnReadsPersistedToolResult(t *testing.T) {
	saved := filepath.Join(t.TempDir(), "tool-results", "b1.txt")
	if err := os.MkdirAll(filepath.Dir(saved), 0o700); err != nil {
		t.Fatal(err)
	}
	full := strings.Repeat("report line\n", 400) + "ask: ran on codex · gpt-5\nask: dispatch " + leafJob + "\n"
	if err := os.WriteFile(saved, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	persisted := "<persisted-output>\nOutput too large (4.9KB). Full output saved to: " + saved +
		"\n\nPreview (first 2KB):\n" + full[:2048] + "\n...\n</persisted-output>"
	in := leafTx(t, "codex", "Objective: x\nAgent: codex\n", persisted)
	writeDispatch(t, leafJob, record(leafJob, "ok", 0))
	if _, blocked := leafStopBlock(in, "a1"); blocked {
		t.Fatal("blocked a leaf whose saved ask output carries the ran-on line past the 2 KB preview")
	}
}

// sleeper starts `sh -c 'exec sleep 30'` in its own process group writing to out; done closes once it is reaped.
func sleeper(t *testing.T, out string) <-chan struct{} {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	cmd.Stdout, cmd.Stderr = f, f
	proc.OwnGroup(cmd) // cancel kills the whole group
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return done
}

func stopped(done <-chan struct{}, within time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// reapFixture returns a leaf stop payload whose transcript names only task A, and both tasks' sleepers.
func reapFixture(t *testing.T, extra ...string) (stop string, a, b <-chan struct{}) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the reaper signals POSIX process groups")
	}
	if inflight.SessionOwner() == 0 {
		t.Skip("no session owner to retire against")
	}
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outA := filepath.Join(dir, "bAAA.output")
	result := "Command running in background with ID: bAAA. Output is being written to: " + outA + ". You will be notified"
	tx := leafEnv(t, append([]string{result}, extra...)...)
	a, b = sleeper(t, outA), sleeper(t, filepath.Join(dir, "bBBB.output"))
	return fmt.Sprintf(`{"agent_id":"a1","transcript_path":%q,"agent_transcript_path":%q}`, tx, tx), a, b
}

// Given a leaf transcript naming task A, when the stop passes, A's group dies and unnamed task B's survives.
func TestSubagentStopReapsOwnBackgroundOnly(t *testing.T) {
	stop, a, b := reapFixture(t)
	Stop(core.ParseInput(strings.NewReader(stop)))
	if !stopped(a, 5*time.Second) {
		t.Fatal("task A's process survived the stop")
	}
	if stopped(b, 200*time.Millisecond) {
		t.Fatal("task B's process (not in the leaf's transcript) was killed")
	}
}

// Given a running dispatch the leaf started, a blocked stop reaps nothing.
func TestSubagentStopBlockedReapsNothing(t *testing.T) {
	stop, a, _ := reapFixture(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	if r := Stop(core.ParseInput(strings.NewReader(stop))); r.Decision != core.Block {
		t.Fatalf("decision = %v, want Block", r.Decision)
	}
	if stopped(a, 300*time.Millisecond) {
		t.Fatal("a blocked stop killed a background process")
	}
}

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
	runLeafEvent(t, "PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","tool_name":"Agent","tool_use_id":"toolu_f1","cwd":"`+repo+`"}`)
	if data, _ := os.ReadFile(plan); strings.Contains(string(data), "working:") {
		t.Fatalf("claim survived a failed spawn:\n%s", data)
	}
}

// Regression (2026-11-10): a gemini leaf that only read docs or this gate's source handed back ungated.
func TestRanOnIgnoresDocAndSourceMentions(t *testing.T) {
	doc := "Envelope (quote the `ask: ran on …` line, outcome first)\nThen `Ran on:` (the `ask: ran on …` line)"
	src := "const relayNote = \"" + relayNote + "\"\nvar ranOnPattern = regexp.MustCompile(`ask: ran on |\\\\\"ran_on\\\\\":`)"
	in := leafTxCmd(t, "gemini", "cat docs.md source.go", "Objective: review internal/hooks/agents\n", doc, src)
	if r, _ := leafStopBlock(in, "a1"); !strings.Contains(r.SystemMessage, relayNote) {
		t.Fatalf("got %+v, want the relay note: prose mentions are not a ran-on line", r)
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

// An expired dispatch note and the receipt gate's cap note both reach the user.
func TestLeafGateKeepsBothNotes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	in := gatedLeafTx(t, "Objective: x\nAgent: codex\n", startedLine())
	in.StopHookActive = true
	writeDispatch(t, leafJob, recordAt(leafJob, "running", os.Getpid(), time.Now().Add(-leafStopMaxWait-time.Minute)))
	r, blocked := leafStopBlock(in, "a1")
	if blocked || !strings.Contains(r.SystemMessage, "still running at the 2h0m0s limit") || !strings.Contains(r.SystemMessage, "limit reached") {
		t.Errorf("got %+v blocked=%v, want one message holding both the ran-on and the expiry notes", r, blocked)
	}
}

func bgNotice(out string) string {
	return "Command was moved to the background (ID: bX1) so that a message that arrived while it was running can reach you; " +
		"it was not interrupted. Output is being written to: " + out + ". You will be notified when it completes."
}

func bgOutputFile(t *testing.T, body string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "tasks", "bX1.output")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRanOnResumedLeafWording(t *testing.T) {
	in := leafTx(t, "codex", "Objective: x\nAgent: codex\n", "ls", handedBackMark)
	appendLines(t, in.AgentTranscriptPath, `{"type":"user","message":{"content":"one more thing"}}`)
	r, blocked := leafStopBlock(in, "a1")
	if blocked || !strings.Contains(r.SystemMessage, "resumed after its hand-back; no new `ask: ran on` receipt this round") {
		t.Fatalf("got %+v blocked=%v, want the resumed wording", r, blocked)
	}
}

// timedLeafTx is a codex leaf transcript whose one ask call (made at `at`) returned result, with its meta.json.
func timedLeafTx(t *testing.T, at time.Time, result string) *core.Input {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	brief, _ := json.Marshal(map[string]any{"type": "user", "timestamp": at.Add(-time.Minute), "message": map[string]any{"content": "Objective: x\nAgent: codex\n"}})
	call, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": `claude-code agents ask --agent codex "x"`}}}}})
	res, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": result}}}})
	tx := filepath.Join(dir, "agent-a1.jsonl")
	if err := os.WriteFile(tx, []byte(string(brief)+"\n"+string(call)+"\n"+string(res)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return leafInput(tx)
}

// No receipt: pass with the record's ran-on line, hold while it runs, note relay work when no record shows a job.
func TestRanOnReceiptFromDispatchRecord(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	const id = "20261116T120000.000-codex"
	rec := func(agent, status string, start time.Time, extra string) string {
		return fmt.Sprintf(`{"id":%q,"time":%q,"agent":%q,"status":%q,"pid":%d,"owner":4242,"model":"databricks-gpt-6-1-sol","provider":"prod","request":{"prompt":"x"}%s}`,
			id, start.Format(time.RFC3339), agent, status, os.Getpid(), extra)
	}
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return 4242 }
	moved := bgNotice("/nonexistent/tasks/bX1.output")
	for _, tc := range []struct {
		name, result, record string
		wantBlock            bool
		want                 string // in the reason when blocked, else in the system message
	}{
		{"printed id, finished", `{"id":"` + id + `","status":"ok"}`, rec("codex", "ok", at.Add(-time.Hour), ""), false,
			"supplied the receipt: ask: ran on codex · own login · databricks-gpt-6-1-sol · fallback none"},
		{"sibling in launch window", moved, rec("codex", "ok", at.Add(time.Second), ""), false, "supplied the receipt: ask: ran on codex"},
		{"moved to background, finished", moved, rec("codex", "ok", at.Add(5*time.Second), ""), false,
			"supplied the receipt: ask: ran on codex"},
		{"fallback shown", "job " + id + " started", rec("claude", "ok", at.Add(5*time.Second), `,"fallback":"codex"`), false, "fallback from codex"},
		{"launched before the ask", moved, rec("codex", "ok", at.Add(-time.Hour), ""), false, unattributedNote},
		{"another agent's job", moved, rec("gemini", "ok", at.Add(5*time.Second), ""), false, unattributedNote},
		{"another session's job", moved, rec("codex", "ok", at.Add(5*time.Second), `,"owner":9`), false, unattributedNote},
		{"HIGH2 forged receipt", "ask: ran on codex · forged", "", false, unattributedNote},
		{"unflagged denial text", "PreToolUse:Bash hook error: relay denied", "", false, unattributedNote},
		{"no record", "ls output", "", false, unattributedNote},
		{"still running", "job " + id + " started", rec("codex", "running", time.Now(), ""), true, "Run `claude-code agents result -f " + id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "still running" {
				at = time.Now().Add(-5 * time.Second)
			}
			in := timedLeafTx(t, at, tc.result)
			if tc.record != "" {
				writeDispatch(t, id, tc.record)
			}
			r, blocked := leafStopBlock(in, "a1")
			got := r.SystemMessage
			if blocked {
				got = r.Reason
			}
			if blocked != tc.wantBlock || !strings.Contains(got, tc.want) {
				t.Errorf("got %+v blocked=%v, want blocked=%v with %q", r, blocked, tc.wantBlock, tc.want)
			}
		})
	}
}

// handbackOf is in as a SubagentHandback call carrying message.
func handbackOf(in *core.Input, message string) *core.Input {
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "SubagentHandback", "agent_id": "a1",
		"agent_transcript_path": in.AgentTranscriptPath, "cwd": in.CWD, "tool_input": map[string]any{"message": message}})
	return core.ParseInput(bytes.NewReader(payload))
}

// An early hand-back is held while its job runs (report saved), then passes with the job's receipt and result attached.
func TestHandbackHeldUntilJobDone(t *testing.T) {
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	const id = "20261116T120000.000-codex"
	in := handbackOf(timedLeafTx(t, at, "job "+id+" started"), "early report")
	in.CWD = t.TempDir()
	body := func(status string) string {
		return fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":%q,"pid":%d,"model":"databricks-gpt-6-1-sol","log":%q}`,
			id, at.Add(3*time.Second).Format(time.RFC3339), status, os.Getpid(), filepath.Join(t.TempDir(), "job.log"))
	}
	writeDispatch(t, id, body("running"))
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	if !blocked || !strings.Contains(r.Reason, "Run `claude-code agents result -f "+id) || !strings.Contains(r.Reason, "is saved at ") {
		t.Fatalf("running = %+v blocked=%v, want a hold naming the saved report", r, blocked)
	}
	if r.Verdict != core.VerdictHeld || r.Rule != "job-running" {
		t.Errorf("held tag = %q/%q, want job-running/held", r.Rule, r.Verdict)
	}
	rec := body("ok")
	writeDispatch(t, id, rec)
	var log struct{ Log string }
	_ = json.Unmarshal([]byte(rec), &log)
	if err := os.WriteFile(log.Log, []byte("job answer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, blocked = leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	msg, _ := r.UpdatedInput["message"].(string)
	if blocked || !strings.HasPrefix(msg, "NOTE: dispatch "+id) || !strings.Contains(msg, "ask: ran on codex") ||
		!strings.Contains(msg, "early report") || !strings.Contains(msg, "job answer") {
		t.Fatalf("finished = %+v blocked=%v, want the hand-back through with note, receipt, report and result", r, blocked)
	}
	if r.Verdict != core.VerdictPassed || r.Rule != "receipt" {
		t.Errorf("passed tag = %q/%q, want receipt/passed", r.Rule, r.Verdict)
	}
}

// writeJobFiles writes record id's log and, when events is set, its events file beside the record.
func writeJobFiles(t *testing.T, id, logText, events string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(logPath, []byte(logText), 0o600); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		p := filepath.Join(paths.StateDir(), "dispatch", id+paths.DispatchEventsSuffix)
		if err := os.WriteFile(p, []byte(events), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return logPath
}

// The attached job result is the job's last message and one summary line: no heartbeat or per-step lines.
func TestHandbackJobResultIsTheAnswer(t *testing.T) {
	const id = "20261116T120000.000-codex"
	noise := "▸ heartbeat: codex still running, no output for 30s\n▸ result: step finished (tool-calls), tokens in=2 out=40\n"
	var tools strings.Builder
	for i := range 25 {
		fmt.Fprintf(&tools, "▸ tool: t%02d\n%s", i, noise)
	}
	events := `{"type":"text","text":"first turn\n"}` + "\n" + `{"type":"text","text":"FINAL line1\n","sep":true}` + "\n" +
		`{"type":"text","text":"line2\n"}` + "\n" + `{"type":"result","envelope":{"answer":"first turn\nFINAL line1\nline2",` +
		`"duration_ms":90000,"usage":{"input_tokens":10,"cache_read_tokens":90,"output_tokens":7}}}` + "\n"
	for _, tc := range []struct {
		name, report, log, events string
		want, never               []string
		answers                   int // times the answer appears in the message
	}{
		{"events last message", "my report", noise + noise + noise + "▸ assistant: draft\n", events,
			[]string{"--- job result ---\ncodex ok · 3 steps · 1m30s · tokens in=100 out=7\n\nFINAL line1\nline2"},
			[]string{"heartbeat", "step finished", "first turn", "draft"}, 1},
		{"plain log answer", "my report", noise + "plain answer\nask: ran on codex · x · y · fallback none\n", "",
			[]string{"codex ok · 1 steps", "plain answer"}, []string{"heartbeat", "step finished", "ask: ran on codex · x"}, 0},
		{"no final text keeps 20 lines", "my report", tools.String(), "",
			[]string{"▸ tool: t05", "▸ tool: t24"}, []string{"heartbeat", "step finished", "▸ tool: t04"}, 0},
		{"report already quotes it", "my report\n\nFINAL line1\nline2\n", noise, events,
			[]string{"(final answer already in the report above)"}, []string{"heartbeat"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-time.Minute).Truncate(time.Second)
			in := handbackOf(timedLeafTx(t, at, "job "+id+" started"), tc.report)
			writeDispatch(t, id, "{}")
			logPath := writeJobFiles(t, id, tc.log, tc.events)
			writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":"ok","pid":%d,"duration_ms":90000,"log":%q}`,
				id, at.Add(3*time.Second).Format(time.RFC3339), os.Getpid(), logPath))
			r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
			msg, _ := r.UpdatedInput["message"].(string)
			for _, w := range tc.want {
				if blocked || !strings.Contains(msg, w) {
					t.Errorf("message lacks %q (blocked=%v):\n%s", w, blocked, msg)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(msg, n) {
					t.Errorf("message carries %q:\n%s", n, msg)
				}
			}
			if tc.answers > 0 && strings.Count(msg, "FINAL line1\nline2") != tc.answers {
				t.Errorf("answer appears %d times, want %d:\n%s", strings.Count(msg, "FINAL line1\nline2"), tc.answers, msg)
			}
		})
	}
}

// A relay that changed files through Bash did the work: the Run: line is not run again; a read-only relay's is.
func TestRanOnRelayBashWrites(t *testing.T) {
	for cmd, wrote := range map[string]bool{
		"echo x > /private/tmp/../../Users/edotau/.claude/internal/hooks/leaves.go": true,
		"echo x > /repo/a.go": true, "sed -i 's/a/b/' a.go": true, "printf x | tee a.go": true, "cp a.go b.go": true,
		"mv a.go b.go": true, "rm -f a.go": true, "git apply fix.patch": true, "cd /repo && git -C /repo rm a.go": true,
		"apply_patch <<'EOF'\n*** Begin Patch\nEOF": true, "gofmt -l -w a.go": true, "timeout 60 go fmt ./...": true,
		"ls -la": false, "cat a.go | grep x": false, "git status && git diff && git log -3": false,
		"go build ./... 2>&1": false, "go test ./... >/dev/null 2>&1": false, "git apply --check fix.patch": false,
		"go test ./... > /tmp/t.out 2>&1": false, "gofmt -l a.go": false, "black a.py": true, "ruff check --fix a.py": true,
		"black --version": false, "isort --help": false, "ruff format --diff a.py": false, "go fmt -n ./...": false,
	} {
		spawned := stubSpawn(t)
		in := handbackOf(leafTxCmd(t, "claude", cmd, "Run: claude-code agents ask --agent codex 'fix it'\n", "out"), "fixed it")
		r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
		msg, _ := r.UpdatedInput["message"].(string)
		if relay := !blocked && len(*spawned) == 0 && strings.HasPrefix(msg, relayNote); relay != wrote {
			t.Errorf("%q: relay note %v (blocked=%v spawns %v), want %v", cmd, relay, blocked, *spawned, wrote)
		}
	}
}

// shrinkHandbackWait sets Handback's poll and budget to test sizes; the record flips to final on the flip-th re-read.
func shrinkHandbackWait(t *testing.T, id string, flip int, final string, readErr bool) {
	t.Helper()
	poll, wait, load := handbackPoll, handbackWait, loadDispatch
	t.Cleanup(func() { handbackPoll, handbackWait, loadDispatch = poll, wait, load })
	handbackPoll, handbackWait = time.Millisecond, 30*time.Millisecond
	calls := 0
	loadDispatch = func(got string) (agent.Record, error) {
		if calls++; got == id && calls > 1 && readErr {
			return agent.Record{}, os.ErrInvalid
		}
		if got == id && calls == flip {
			writeDispatch(t, id, final)
		}
		return load(got)
	}
}

// A hand-back waits on its running job: one that ends in the budget passes with the result; else today's paths.
func TestHandbackWaitsOnJob(t *testing.T) {
	ranOn := `,"agent":"codex","model":"databricks-gpt-6-1-sol"`
	for _, tc := range []struct {
		name, result string
		flip         int // re-read on which the record turns ok; 0 never
		readErr      bool
		wantHold     bool
		want         string
	}{
		{"ends inside the budget", startedLine(), 3, false, false, "NOTE: dispatch " + leafJob + " (ended while the hand-back waited)"},
		{"still running at the budget", startedLine(), 0, false, true, "claude-code agents result -f " + leafJob},
		{"no job", "ls output", 0, false, false, ""},
		{"record unreadable", startedLine(), 0, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := handbackOf(leafInput(leafEnv(t, tc.result)), "early report")
			running := strings.TrimSuffix(record(leafJob, "running", os.Getpid()), "}") + ranOn + "}"
			writeDispatch(t, leafJob, running)
			shrinkHandbackWait(t, leafJob, tc.flip, strings.Replace(running, `"running"`, `"ok"`, 1), tc.readErr)
			r := Handback(in)
			msg, _ := r.UpdatedInput["message"].(string)
			if hold := r.Decision == core.Block; hold != tc.wantHold || !strings.Contains(r.Reason+msg, tc.want) ||
				(!hold && tc.want != "" && !strings.Contains(msg, "early report")) {
				t.Fatalf("got %+v, want hold=%v with %q and the report kept", r, tc.wantHold, tc.want)
			}
		})
	}
}

// An external leaf's job found by launch time ends in the budget: the receipt is attached; an unreadable one notes.
func TestHandbackWaitsOnReceiptJob(t *testing.T) {
	const id = "20261116T120000.000-codex"
	for _, tc := range []struct {
		name    string
		readErr bool
		want    string
	}{
		{"ends inside the budget", false, "ask: ran on codex"},
		{"record unreadable", true, "became unreadable while the hand-back waited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().Add(-time.Minute).Truncate(time.Second)
			in := handbackOf(timedLeafTx(t, at, "job "+id+" started"), "early report")
			body := func(status string) string {
				return fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":%q,"pid":%d,"model":"databricks-gpt-6-1-sol"}`,
					id, at.Add(3*time.Second).Format(time.RFC3339), status, os.Getpid())
			}
			writeDispatch(t, id, body("running"))
			shrinkHandbackWait(t, id, 2, body("ok"), tc.readErr)
			r := Handback(in)
			msg, _ := r.UpdatedInput["message"].(string)
			if r.Decision == core.Block || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "early report") {
				t.Fatalf("got %+v, want the hand-back through with %q and the report", r, tc.want)
			}
		})
	}
}

// The hand-back wait ends a HookMargin before the dispatcher cuts dispatch-handback.
func TestHandbackWaitBudget(t *testing.T) {
	spec, _ := core.SpecOf("dispatch-handback")
	if handbackWait <= 0 || handbackWait+core.HookMargin > spec.Budget {
		t.Fatalf("handbackWait = %s with budget %s, want 0 < wait <= budget-%s", handbackWait, spec.Budget, core.HookMargin)
	}
}

// stubTwoJobs serves job "first" (ok) and "second" (running on its first read and while *hold), each with its answer.
func stubTwoJobs(t *testing.T, in *core.Input) (reads *int, hold *bool) {
	orig := loadDispatch
	t.Cleanup(func() { loadDispatch = orig })
	reads, hold = new(int), new(bool)
	loadDispatch = func(id string) (agent.Record, error) {
		status := "ok"
		if id == "second" {
			if *reads++; *reads == 1 || *hold {
				status = "running"
			}
		}
		log := filepath.Join(filepath.Dir(in.AgentTranscriptPath), id+".log")
		if err := os.WriteFile(log, []byte("FINAL ANSWER "+id), 0o600); err != nil {
			t.Fatal(err)
		}
		return agent.Record{Log: log, ID: id, Agent: "codex", Status: status, PID: os.Getpid(), Time: time.Now().Add(-time.Minute)}, nil
	}
	return reads, hold
}

// Two jobs, one ending in the wait, with or without a ran-on line: both answers ride along in order, the waited on top.
func TestHandbackKeepsEveryFinishedAnswer(t *testing.T) {
	poll := handbackPoll
	t.Cleanup(func() { handbackPoll = poll })
	handbackPoll = time.Millisecond
	for _, ranOn := range []bool{false, true} {
		in := handbackOf(leafTx(t, "codex", "Objective: x\nAgent: codex\n", "ls output"), "relay report")
		reads, _ := stubTwoJobs(t, in)
		lr := readLeaf(in.AgentTranscriptPath, leafWants(in))
		lr.scan.ranOn, lr.ids, lr.until = ranOn, []string{"first", "second"}, time.Now().Add(time.Second)
		r, blocked := leafGate(in, "a1", lr)
		msg, _ := r.UpdatedInput["message"].(string)
		first, second := strings.Index(msg, "FINAL ANSWER first"), strings.Index(msg, "FINAL ANSWER second")
		if blocked || *reads < 2 || first < 0 || second < first || !strings.HasPrefix(msg, "NOTE: dispatch second ") {
			t.Fatalf("ranOn=%v: got %+v (reads=%d), want both answers in dispatch order under the second's receipt", ranOn, r, *reads)
		}
	}
}

// Regression (2026-11-16): ids in a listing the leaf ran, or a sibling's job started near its ask, are never its own.
func TestHandbackIgnoresForeignDispatches(t *testing.T) {
	const own, b, c = "20261116T180732.880-opencode", "20261116T180927.796-opencode", "20261116T180930.100-opencode"
	listing := `{"id":"` + b + `","status":"ok"}` + "\njob " + c + " started\nask: dispatch " + c + "\n"
	for _, list := range []bool{false, true} {
		t.Run(fmt.Sprintf("listing=%v", list), func(t *testing.T) {
			at := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			msg := func(typ string, ts time.Time, content any) string {
				line, _ := json.Marshal(map[string]any{"type": typ, "timestamp": ts, "message": map[string]any{"content": content}})
				return string(line) + "\n"
			}
			call := func(id, cmd string, ts time.Time) string {
				return msg("assistant", ts, []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}})
			}
			result := func(id, text string) string {
				return msg("user", at, []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text}})
			}
			body := msg("user", at.Add(-time.Minute), "Objective: x\nAgent: opencode\n") +
				call("t1", `claude-code agents ask --agent opencode "x"`, at) + result("t1", bgNotice(bgOutputFile(t, "working\n")))
			if list {
				body += call("t2", "claude-code agents status --format json", at.Add(2*time.Minute)) + result("t2", listing)
			}
			tx := filepath.Join(dir, "agent-a1.jsonl")
			if err := os.WriteFile(tx, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"opencode"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			for id, start := range map[string]time.Duration{own: time.Second, b: 95 * time.Second, c: 98 * time.Second} {
				status := map[bool]string{true: "running", false: "ok"}[id == own]
				logPath := writeJobFiles(t, id, "ANSWER "+id+"\n", "")
				writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"opencode","status":%q,"pid":%d,"model":"m","log":%q}`,
					id, at.Add(start).Format(time.RFC3339), status, os.Getpid(), logPath))
			}
			shrinkHandbackWait(t, own, 0, "", false)
			r := Handback(handbackOf(leafInput(tx), "relay report"))
			msg2, _ := r.UpdatedInput["message"].(string)
			all := r.Reason + r.SystemMessage + msg2
			if r.Decision == core.Block || !strings.Contains(all, unattributedNote) || strings.Contains(all, own) || strings.Contains(all, b) || strings.Contains(all, c) {
				t.Fatalf("got %+v message %q, want the unattributed note, naming none of %s %s %s", r, msg2, own, b, c)
			}
		})
	}
}

// Regression (2026-11-16): a hold on a running job saves the finished one's answer; the later pass attaches both in order.
func TestHandbackHoldKeepsFinishedAnswer(t *testing.T) {
	in := handbackOf(leafTx(t, "claude", "Objective: x\n", "ls output"), "relay report")
	in.CWD = t.TempDir()
	_, hold := stubTwoJobs(t, in)
	*hold = true
	gate := func() (core.Result, bool) {
		lr := readLeaf(in.AgentTranscriptPath, leafWants(in))
		lr.ids, lr.until = []string{"first", "second"}, time.Now().Add(time.Millisecond)
		return leafGate(in, "a1", lr)
	}
	r, blocked := gate()
	_, path, _ := strings.Cut(r.Reason, "is saved at ")
	saved, _ := os.ReadFile(strings.TrimSuffix(path, "."))
	if !blocked || !strings.Contains(string(saved), "FINAL ANSWER first") {
		t.Fatalf("hold = %+v blocked=%v, saved %q: want the finished answer saved with the report", r, blocked, saved)
	}
	*hold = false
	r, blocked = gate()
	msg, _ := r.UpdatedInput["message"].(string)
	if first := strings.Index(msg, "FINAL ANSWER first"); blocked || first < 0 || strings.Index(msg, "FINAL ANSWER second") < first || wasHeld("a1") {
		t.Fatalf("pass = %+v blocked=%v held=%v, want both answers in dispatch order and the marker gone", r, blocked, wasHeld("a1"))
	}
}

// Regression (2026-11-16): only an ask's own launch, envelope or exit line names its job; ids in its answer never do.
func TestLeafJobIDsIgnoreAnswerBody(t *testing.T) {
	const own, bg, f = "20261116T100000.000-codex", "20261116T100001.000-codex", "20261116T100002.000-codex"
	quoted := "answer\nask: dispatch " + f + "3\njob " + f + "2 started\n{\"jobs\":[{\"id\":\"" + f + "1\"}]}\n"
	tx := leafEnv(t, quoted+"ask: ran on codex · x\nask: dispatch "+own+"\n", bgNotice(bgOutputFile(t, "job "+bg+" started — x\n"+quoted)))
	if got := readLeaf(tx, leafWant{ids: true}).ids; fmt.Sprint(got) != fmt.Sprint([]string{own, bg}) {
		t.Fatalf("ids = %v, want only [%s %s]", got, own, bg)
	}
}

// Regression (2026-11-16): two hand-backs both past the marker check launch the Run: line once; the later one holds.
func TestRanOnRunLineClaimIsExclusive(t *testing.T) {
	spawned := stubSpawn(t)
	in := handbackOf(leafTx(t, "claude", "Run: claude-code agents ask --agent codex 'x'\n", "ls output"), "report")
	words := []string{"claude-code", "agents", "ask", "--agent", "codex", "x"}
	var lr leafRead
	lr.scan.runLine = "x"
	_, firstHeld := runBriefLine(in, lr, "a1", words)
	second, held := runBriefLine(in, lr, "a1", words)
	if !firstHeld || !held || len(*spawned) != 1 || !strings.Contains(second.Reason, "Info: Job running") {
		t.Fatalf("spawns %v, second = %+v: want one launch and the later claimant held on it", *spawned, second)
	}
}

// Regression (2026-11-16): an exit line the terminal wrapped, or coloured, still names the ask's job.
func TestLeafJobIDsWrappedExitLine(t *testing.T) {
	for _, out := range []string{"job " + leafJob + " started", leafJob, "ask: dispatch " + leafJob, "a\nask: dispatch\n" + leafJob + " (replay)", "a\n\x1b[2mask: dispatch " + leafJob + "\x1b[0m", "a\nask: dispatch " + leafJob + "\n\n[exited with code 0]"} {
		if got := readLeaf(leafEnv(t, out), leafWant{ids: true}).ids; fmt.Sprint(got) != fmt.Sprint([]string{leafJob}) {
			t.Errorf("%q: ids = %v, want [%s]", out, got, leafJob)
		}
	}
}

// A hand-back while the hook's own launch is inside askLaunchWindow with no receipt yet is held, not released.
func TestRanOnHookLaunchPendingHolds(t *testing.T) {
	spawned := stubSpawn(t)
	in := handbackOf(leafTx(t, "claude", "Run: claude-code agents ask --agent codex 'x'\n", "ls output"), "report")
	leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in))) // the hook launches; its output stays empty
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	if !blocked || len(*spawned) != 1 || r.UpdatedInput != nil ||
		!strings.Contains(r.Reason, "Info: Job running") || !strings.Contains(r.Reason, "hand-back waits") {
		t.Fatalf("got %+v blocked=%v spawns %v, want a hold on the pending launch and no new launch", r, blocked, *spawned)
	}
}

// A hand-back whose job is still running past leafStopMaxWait passes with the limit note instead of holding.
func TestHandbackReleasesPastWaitLimit(t *testing.T) {
	const id = "20261116T120000.000-codex"
	at := time.Now().Add(-leafStopMaxWait - time.Hour).Truncate(time.Second)
	in := handbackOf(timedLeafTx(t, at, "job "+id+" started"), "early report")
	writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":"running","pid":%d}`,
		id, at.Add(3*time.Second).Format(time.RFC3339), os.Getpid()))
	r := Handback(in)
	msg, _ := r.UpdatedInput["message"].(string)
	if r.Decision == core.Block || !strings.Contains(msg, "still running at the "+leafStopMaxWait.String()+" limit") ||
		!strings.Contains(msg, "early report") {
		t.Fatalf("got %+v, want the hand-back through with the limit note and the report", r)
	}
}

// An ask whose answer merely quotes "PreToolUse … denied" still ran: its record binds and the hook launches nothing.
func TestQuotedHookDenialIsNoDeniedAsk(t *testing.T) {
	const id = "20261116T120000.000-codex"
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return 4242 }
	spawned := stubSpawn(t)
	at := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	result := "The ask check ran through the PreToolUse preflight hook and was denied with the hint.\nask: ran on codex · own login · m · fallback none"
	tx := timedLeafTx(t, at, result)
	b, err := os.ReadFile(tx.AgentTranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`Objective: x\nAgent: codex\n`), []byte(`Run: claude-code agents ask --agent codex \"x\"\n\nObjective: x\n`), 1)
	if err := os.WriteFile(tx.AgentTranscriptPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	in := handbackOf(tx, "relay report")
	in.CWD = t.TempDir()
	writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":"ok","pid":%d,"owner":4242,"model":"m","request":{"prompt":"x"}}`,
		id, at.Add(time.Second).Format(time.RFC3339), os.Getpid()))
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	msg, _ := r.UpdatedInput["message"].(string)
	if blocked || len(*spawned) != 0 || !strings.Contains(msg, "NOTE: dispatch "+id) {
		t.Fatalf("got %+v blocked=%v spawns %v, want the record's receipt and no launch", r, blocked, *spawned)
	}
}

func TestLeafDenialProvenance(t *testing.T) {
	for _, text := range []string{
		"The report mentions PreToolUse checks were denied in the reviewed example",
		"Report:\nPreToolUse checks were denied in the reviewed example",
		"```\nPreToolUse:Bash hook error: denied\n```",
		"PreToolUse checks were denied in the reviewed example",
		`{"permissionDecision":"deny"}`,
	} {
		t.Run(text, func(t *testing.T) {
			spawned := stubSpawn(t)
			in := timedLeafTx(t, time.Now().Add(-time.Minute), text)
			appendLines(t, in.AgentTranscriptPath, txCall("other", "Bash", "ls"), `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"other","is_error":true,"content":"PreToolUse:Bash hook error: denied"}]}}`)
			lr := readLeaf(in.AgentTranscriptPath, leafWants(in), in.CWD)
			if len(lr.scan.askTimes) != 1 {
				t.Fatalf("successful ask lost: %+v", lr.scan)
			}
			lr.scan.runLine = `claude-code agents ask --agent codex "x"`
			r, blocked := leafGate(in, "a1", lr)
			if blocked || len(*spawned) != 0 || !strings.Contains(r.SystemMessage, unattributedNote) {
				t.Fatalf("ambiguous result: %+v blocked=%v", r, blocked)
			}
		})
	}
}
