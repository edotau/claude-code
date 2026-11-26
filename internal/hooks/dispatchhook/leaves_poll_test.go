package dispatchhook

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
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

// A relay's own successful `agents result|wait <id>` after a backgrounded ask binds an id whose record is its ask's.
func TestHandbackBindsOwnPoll(t *testing.T) {
	const id = "20261117T101000.000-opencode"
	const owner = 4242
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return owner } // a fixed session pid: the ambient one is 0 outside a Claude session
	at := time.Now().Add(-15 * time.Minute).Truncate(time.Second)
	good := "▸ result: " + id + "\nANSWER\nask: ran on opencode · m"
	res, wait := "claude-code agents result -f "+id, "claude-code agents wait "+id+" --timeout 590s"
	for _, c := range []pollRow{
		{name: "result -f", cmd: res, want: true},
		{name: "wait", cmd: wait, want: true},
		{name: "brief file ask", cmd: res, file: true, want: true},
		{name: "brief file of another prompt", cmd: res, file: true, prompt: "audit beta"},
		{name: "rtk wait", cmd: "rtk " + wait, want: true},
		{name: "tool error", cmd: res, isErr: true},
		{name: "hook denial text", cmd: res, isErr: true, out: "PreToolUse:Bash hook error: blocked"},
		{name: "permissionDecision deny", cmd: res, isErr: true, out: `{"permissionDecision":"deny"}`},
		{name: "no record", cmd: res, noRec: true},
		{name: "another session's record", cmd: res, own: owner + 1},
		{name: "record owner 0", cmd: res, own: -1},
		{name: "record before the ask", cmd: res, start: -time.Second},
		{name: "poll with no prior ask", cmd: res, noAsk: true},
		{name: "launched after the window", cmd: res, start: askLaunchWindow + time.Second},
		{name: "sibling's prompt", cmd: res, prompt: "audit beta"},
		{name: "sibling's agent", cmd: res, agent: "codex"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.runPollRow(t, id, good, at, owner)
		})
	}
}

// pollRow is one case of TestHandbackBindsOwnPoll; a zero field is the leaf's own ask's value.
type pollRow struct {
	name, cmd, out, agent, prompt string
	isErr, noRec, want, file      bool
	noAsk                         bool
	own                           int
	start                         time.Duration
}

func (c pollRow) runPollRow(t *testing.T, id, good string, at time.Time, owner int) {
	sess := relayTx(t, "")
	out := cmp.Or(c.out, good)
	ask := `claude-code agents ask --agent opencode "audit alpha"`
	if c.noAsk {
		ask = ""
	}
	if c.file {
		brief := filepath.Join(t.TempDir(), "brief.md")
		if err := os.WriteFile(brief, []byte("audit alpha\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ask = "claude-code agents ask --agent opencode --background - < " + brief
	}
	tx := filepath.Join(strings.TrimSuffix(sess, ".jsonl"), "subagents", "agent-a1.jsonl")
	if err := os.WriteFile(tx, []byte(pollTranscript(t, at, ask, c.cmd, out, c.isErr)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.noRec {
		writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":%q,"status":"ok","owner":%d,"model":"m","log":%q,"request":{"Prompt":%q}}`,
			id, at.Add(cmp.Or(c.start, time.Second)).Format(time.RFC3339), cmp.Or(c.agent, "opencode"), max(cmp.Or(c.own, owner), 0),
			writeJobFiles(t, id, good+"\n", ""), cmp.Or(c.prompt, "audit alpha")))
	}
	if got := readLeaf(tx, leafWant{ids: true}).ids; slices.Contains(got, id) != c.want {
		t.Errorf("ids %v, want %s bound=%v", got, id, c.want)
	}
	if c.noAsk {
		return // with no ask at all the hand-back passes as relay work, not by this binding
	}
	r := Handback(handbackOf(leafInput(tx), "relay report"))
	if r.Decision == core.Block {
		t.Errorf("hand-back %+v, want it accepted (bound=%v)", r, c.want)
	}
}

// pollTranscript is a relay's timed transcript: a backgrounded ask at `at`, then the poll `cmd` ten minutes later.
func pollTranscript(t *testing.T, at time.Time, ask, cmd, out string, isErr bool) string {
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
	launch := use("t1", ask, at) + res("t1", bgNotice(bgOutputFile(t, "working\n")), false)
	if ask == "" {
		launch = ""
	}
	return msg("user", at.Add(-time.Minute), "Objective: x\nAgent: opencode\n") + launch +
		use("t2", cmd, at.Add(10*time.Minute)) + res("t2", out, isErr)
}

// reapGroups never targets our own, the parent's, init's, our pid's or a failed probe's group, and lists each once.
func TestReapGroupsSkipsProtectedAndDuplicates(t *testing.T) {
	origOwn, origOf := reapOwnGroup, reapGroupOf
	t.Cleanup(func() { reapOwnGroup, reapGroupOf = origOwn, origOf })
	groups := map[int]int{os.Getppid(): 200, 11: 100, 12: 200, 13: 1, 14: 300, 15: 300, os.Getpid(): 400, 17: 500}
	reapOwnGroup = func() int { return 100 }
	reapGroupOf = func(pid int) (int, error) {
		if g, ok := groups[pid]; ok {
			return g, nil
		}
		return 0, syscall.ESRCH
	}
	got := reapGroups([]int{11, 12, 13, 14, 15, 16, os.Getpid(), 17})
	if want := []int{300, 500}; !slices.Equal(got, want) {
		t.Fatalf("reapGroups = %v, want %v", got, want)
	}
}

// terminateGroups sends SIGTERM, polls through the grace period, then SIGKILLs only when a group outlived it.
func TestTerminateGroupsEscalatesToKill(t *testing.T) {
	origSignal, origSleep := reapSignal, reapSleep
	t.Cleanup(func() { reapSignal, reapSleep = origSignal, origSleep })
	reapSleep = func(time.Duration) {}
	for _, tc := range []struct {
		name     string
		stubborn bool
		want     []string
	}{
		{"dies on TERM", false, []string{"7:15", "8:15"}},
		{"outlives grace", true, []string{"7:15", "8:15", "7:9", "8:9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			reapSignal = func(g int, sig syscall.Signal) error {
				if sig == 0 {
					if tc.stubborn {
						return nil
					}
					return syscall.ESRCH
				}
				sent = append(sent, fmt.Sprintf("%d:%d", g, sig))
				return nil
			}
			terminateGroups([]int{7, 8})
			if !slices.Equal(sent, tc.want) {
				t.Fatalf("signals = %v, want %v", sent, tc.want)
			}
		})
	}
}

func TestAppendOutputFilesMovedToBackground(t *testing.T) {
	out := "/tmp/c/tasks/bX1.output"
	for _, text := range []string{bgNotice(out), "Command running in background with ID: bX1. Output is being written to: " + out + ". You"} {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t", "content": text}}}})
		if got := appendOutputFiles(nil, map[string]bool{}, line); len(got) != 1 || got[0] != out {
			t.Errorf("%.40s...: got %v, want [%s]", text, got, out)
		}
	}
}
