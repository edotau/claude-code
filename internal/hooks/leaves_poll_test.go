package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
)

const pollJobID = "20261116T120000.000-opencode"

// relayTx writes opencode leaf a1's transcript where the session's hooks find it and returns the session transcript.
func relayTx(t *testing.T, lines ...string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sess := filepath.Join(t.TempDir(), "sess.jsonl")
	dir := filepath.Join(strings.TrimSuffix(sess, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	launch := []string{txCall("t0", "Bash", "claude-code agents ask --agent opencode --background - < b.md"),
		txResult("t0", "job "+pollJobID+" started")}
	body := strings.Join(append(launch, lines...), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"opencode"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return sess
}

func txCall(id, tool, command string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": id, "name": tool, "input": map[string]any{"command": command}}}}})
	return string(b)
}

func txResult(id, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_result", "tool_use_id": id, "content": text}}}})
	return string(b)
}

// bashHook is leaf a1's Bash hook payload for one call; an empty agent is the main session's.
func bashHook(event, sess, agent, toolUseID, command, stdout string) *core.Input {
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "tool_name": "Bash", "agent_id": agent, "transcript_path": sess,
		"tool_use_id": toolUseID, "tool_input": map[string]any{"command": command}, "tool_response": map[string]any{"stdout": stdout}})
	return core.ParseInput(strings.NewReader(string(b)))
}

// (a) A relay's own poll that returns on an ended job is told to hand back; a running job or the main session is not.
func TestPollNudgeOnEndedJob(t *testing.T) {
	sess := relayTx(t)
	poll := "timeout 590 claude-code agents result -f 20261116T120000.000 2>&1 | tail -60"
	writeDispatch(t, pollJobID, record(pollJobID, "running", os.Getpid()))
	if r := AfterTool(bashHook("PostToolUse", sess, "a1", "t1", poll, "still going")); r.Context != "" {
		t.Fatalf("running job: context %q, want none", r.Context)
	}
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	if r := AfterTool(bashHook("PostToolUse", sess, "", "t1", poll, "done")); r.Context != "" {
		t.Fatalf("main session: context %q, want none", r.Context)
	}
	want := "job " + pollJobID + " ended: hand back now with this result"
	if r := AfterTool(bashHook("PostToolUse", sess, "a1", "t1", poll, "answer\nask: ran on opencode · x")); r.Context != want {
		t.Fatalf("ended job: context %q, want %q", r.Context, want)
	}
}

// (b) After a delivered hand-back, a poll of the same finished job is refused with "already reported: stop".
func TestPollGateAlreadyReported(t *testing.T) {
	poll := "claude-code agents result -f " + pollJobID
	polled := []string{txCall("t1", "Bash", poll), txResult("t1", "answer\nask: ran on opencode · x")}
	handback := []string{txCall("h1", "SubagentHandback", ""), txResult("h1", `{"success":true,"message":"Report delivered to your caller."}`)}
	sess := relayTx(t, polled...)
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	if r := Handback(bashHook("PreToolUse", sess, "a1", "t2", poll, "")); r.Decision == core.Block {
		t.Fatalf("not yet handed back: %+v, want the poll allowed", r)
	}
	sess = relayTx(t, append(polled, handback...)...)
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	r := Handback(bashHook("PreToolUse", sess, "a1", "t2", poll, ""))
	if r.Decision != core.Block || !strings.HasPrefix(r.Reason, "already reported: stop") {
		t.Fatalf("handed back: %+v, want a block starting \"already reported: stop\"", r)
	}
}

// (c) A second follow poll of one job is refused while the first is in flight or backgrounded with a live writer.
func TestPollGateOnePoller(t *testing.T) {
	poll := "claude-code agents result -f " + pollJobID
	out := bgOutputFile(t, "")
	holders := []int{}
	orig := reapHolders
	t.Cleanup(func() { reapHolders = orig })
	reapHolders = func(files []string) []int {
		if len(files) == 1 && files[0] == out {
			return holders
		}
		return nil
	}
	for _, c := range []struct {
		name    string
		lines   []string
		holders []int
		block   bool
		command string
	}{
		{"HIGH1 output redirect", nil, nil, true, "claude-code agents ask x > /some/file.go"},
		{"in flight", []string{txCall("t1", "Bash", poll)}, nil, true, ""},
		{"HIGH4 nested wrappers", []string{txCall("t1", "Bash", "rtk env -i timeout -k 2 30 rtk "+poll)}, nil, true, ""},
		{"in flight, rtk-prefixed", []string{txCall("t1", "Bash", "rtk "+poll)}, nil, true, ""},
		{"backgrounded, writer alive", []string{txCall("t1", "Bash", poll), txResult("t1", bgNotice(out))}, []int{4242}, true, ""},
		{"backgrounded, writer gone", []string{txCall("t1", "Bash", poll), txResult("t1", bgNotice(out))}, nil, false, ""},
		{"returned", []string{txCall("t1", "Bash", poll), txResult("t1", "▸ heartbeat: x")}, nil, false, ""},
	} {
		sess := relayTx(t, c.lines...)
		writeDispatch(t, pollJobID, record(pollJobID, "running", os.Getpid()))
		holders = c.holders
		command := poll
		if c.command != "" {
			command = c.command
		}
		r := Handback(bashHook("PreToolUse", sess, "a1", "t2", command, ""))
		if got := r.Decision == core.Block && (strings.HasPrefix(r.Reason, "one poller only") || c.command != ""); got != c.block {
			t.Errorf("%s: %+v, want block=%v", c.name, r, c.block)
		}
	}
}

func TestRelaySyntax(t *testing.T) {
	for _, cmd := range []string{
		"claude-code agents ask x > /some/file.go",
		"claude-code agents ask $(printf x)",
		"claude-code agents ask `printf x`",
		"claude-code agents ask <(printf x)",
		"claude-code agents ask x 2>&1",
	} {
		sess := relayTx(t)
		if r := Handback(bashHook("PreToolUse", sess, "a1", "t2", cmd, "")); r.Decision != core.Block {
			t.Errorf("HIGH1 %q allowed: %+v", cmd, r)
		}
	}
	r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "claude-code agents ask $(printf x)", ""))
	if want := "refused `claude-code agents ask $(printf x)`: contains `$` or a backtick"; !strings.Contains(r.Reason, want) {
		t.Errorf("reason %q, want it to contain %q", r.Reason, want)
	}
}

// A relay's own successful `agents result|wait <id>` after a backgrounded ask binds an id whose record is the leaf's.
func TestHandbackBindsOwnPoll(t *testing.T) {
	const id = "20261117T101000.000-opencode"
	owner, at := inflight.SessionOwner(), time.Now().Add(-15*time.Minute).Truncate(time.Second)
	out := "▸ result: " + id + "\nANSWER\nask: ran on opencode · m"
	for _, c := range []struct {
		name, cmd string
		isErr     bool
		own       int           // the record's Owner
		start     time.Duration // the record's start after the ask call; a negative one writes no record
		want      bool
	}{
		{"result -f", "claude-code agents result -f " + id, false, owner, time.Second, true},
		{"wait", "claude-code agents wait " + id + " --timeout 590s", false, owner, time.Second, true},
		{"rtk wait", "rtk claude-code agents wait " + id + " --timeout 590s", false, owner, time.Second, true},
		{"tool error", "claude-code agents result -f " + id, true, owner, time.Second, false},
		{"no record", "claude-code agents result -f " + id, false, owner, -1, false},
		{"another session's record", "claude-code agents result -f " + id, false, owner + 1, time.Second, false},
		{"launched after the window", "claude-code agents result -f " + id, false, owner, askLaunchWindow + time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sess := relayTx(t, "")
			body := pollTranscript(t, at, c.cmd, out, c.isErr)
			tx := filepath.Join(strings.TrimSuffix(sess, ".jsonl"), "subagents", "agent-a1.jsonl")
			if err := os.WriteFile(tx, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.start >= 0 {
				writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"opencode","status":"ok","owner":%d,"model":"m","log":%q}`,
					id, at.Add(c.start).Format(time.RFC3339), c.own, writeJobFiles(t, id, out+"\n", "")))
			}
			if got := readLeaf(tx, leafWant{ids: true}).ids; slices.Contains(got, id) != c.want {
				t.Errorf("ids %v, want %s bound=%v", got, id, c.want)
			}
			r := Handback(handbackOf(leafInput(tx), "relay report"))
			if blocked := r.Decision == core.Block && strings.Contains(r.Reason, "exact dispatch id"); blocked == c.want {
				t.Errorf("hand-back %+v, want accepted=%v", r, c.want)
			}
		})
	}
}

// pollTranscript is a relay's timed transcript: a backgrounded ask at `at`, then the poll `cmd` ten minutes later.
func pollTranscript(t *testing.T, at time.Time, cmd, out string, isErr bool) string {
	t.Helper()
	msg := func(typ string, ts time.Time, content any) string {
		line, _ := json.Marshal(map[string]any{"type": typ, "timestamp": ts, "message": map[string]any{"content": content}})
		return string(line) + "\n"
	}
	use := func(id, cmd string, ts time.Time) string {
		return msg("assistant", ts, []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}})
	}
	res := func(id, text string, isErr bool) string {
		return msg("user", at, []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}})
	}
	return msg("user", at.Add(-time.Minute), "Objective: x\nAgent: opencode\n") +
		use("t1", `claude-code agents ask --agent opencode "x"`, at) + res("t1", bgNotice(bgOutputFile(t, "working\n")), false) +
		use("t2", cmd, at.Add(10*time.Minute)) + res("t2", out, isErr)
}
