package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc/inflight"
)

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

func leafInput(tx string) *Input { return &Input{AgentTranscriptPath: tx} }

func startedLine() string { return "job " + leafJob + " started" }

// A running dispatch blocks the stop, naming the id and the tail command, and the Stop path exits 2 with the reason.
func TestLeafStopBlocksRunningDispatch(t *testing.T) {
	tx := leafEnv(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	r, blocked := leafStopBlock(leafInput(tx), "a1")
	if !blocked || r.Decision != Block || !strings.Contains(r.Reason, leafJob) ||
		!strings.Contains(r.Reason, "claude-code agents tail -f "+leafJob) || !strings.Contains(r.Reason, "600000ms") {
		t.Fatalf("got %+v blocked=%v, want a Block naming %s, the tail command and the 600000ms timeout", r, blocked, leafJob)
	}
	var out, errb bytes.Buffer
	if code := Emit(VendorClaude, "SubagentStop", r, &out, &errb); code != ExitBlock || !strings.Contains(errb.String(), leafJob) {
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
			if r, blocked := leafStopBlock(leafInput(tx), "a1"); blocked || r.Decision == Block {
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
	if blocked || r.Decision == Block || !strings.Contains(r.SystemMessage, leafJob) || !strings.Contains(r.SystemMessage, leafStopMaxWait.String()) {
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
	in := leafInput(tx)
	in.vendor = VendorCopilot
	if _, blocked := leafStopBlock(in, "a1"); blocked {
		t.Fatal("non-Claude vendor was gated")
	}
}

// A blocked stop keeps the leaf's inflight slot; the stop that proceeds retires it.
func TestLeafStopBlockKeepsSlot(t *testing.T) {
	dispatchEnv(t)
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	sub := inflight.SubagentDir(tx)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-a1.meta.json"), []byte(`{"toolUseId":"u1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agentTx := filepath.Join(sub, "agent-a1.jsonl")
	line, _ := json.Marshal(map[string]any{"type": "tool_result", "content": startedLine()})
	if err := os.WriteFile(agentTx, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	if r := pre(spawnPayload("u1")); r.Decision == Block {
		t.Fatalf("spawn blocked: %s", r.Reason)
	}
	stop := `{"hook_event_name":"SubagentStop","agent_id":"a1","transcript_path":"` + tx + `","agent_transcript_path":"` + agentTx + `"}`
	if r := subagentStopRunner(ParseInput(strings.NewReader(stop))); r.Decision != Block {
		t.Fatalf("running dispatch: got %+v, want Block", r)
	}
	if n := len(markerNames(t)); n != 1 {
		t.Fatalf("blocked stop left %d markers, want 1 (slot kept)", n)
	}
	writeDispatch(t, leafJob, record(leafJob, "done", os.Getpid()))
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
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
