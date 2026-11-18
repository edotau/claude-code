package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

type fakeLauncher struct{ bin string }

func (f fakeLauncher) Claude(context.Context, agent.ClaudeOptions, []string) (agent.ClaudeLaunch, error) {
	return agent.ClaudeLaunch{Binary: f.bin}, nil
}

// runFakeClaude runs claudeExec against a shell script standing in for claude, with an idle limit far above start-up.
func runFakeClaude(t *testing.T, script string) (wire.Result, error) {
	t.Helper()
	return runFakeClaudeIdle(t, 5*time.Second, script)
}

type fakeRun struct {
	res         wire.Result
	err         error
	limit, took time.Duration // of the final attempt
}

func fixed(script string) func(time.Duration) string {
	return func(time.Duration) string { return script }
}

// runFakeClaudeStall retries with a doubled limit (to 4.8 s) while the child was too slow to emit its first event.
func runFakeClaudeStall(t *testing.T, script func(limit time.Duration) string) fakeRun {
	t.Helper()
	limit := 150 * time.Millisecond
	for {
		start := time.Now()
		res, err := runFakeClaudeIdle(t, limit, script(limit))
		slow := errors.Is(err, agent.ErrStalled) && strings.Contains(err.Error(), "last event: none")
		if !slow || limit > 3*time.Second {
			return fakeRun{res, err, limit, time.Since(start)}
		}
		limit *= 2
	}
}

func runFakeClaudeIdle(t *testing.T, limit time.Duration, script string) (wire.Result, error) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	old := claudeIdleLimit
	claudeIdleLimit = limit
	t.Cleanup(func() { claudeIdleLimit = old })
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return claudeAgent.Run(ctx, agent.RunOptions{Prompt: "x", Tools: true, Launcher: fakeLauncher{bin}}, wire.Discard)
}

const (
	initLine   = `{"type":"system","subtype":"init","session_id":"s","model":"m"}`
	toolUse    = `{"type":"assistant","session_id":"s","message":{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`
	toolResult = `{"type":"user","session_id":"s","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	resultLine = `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s"}`
)

// A claude that goes silent after system/init is a stalled failure naming the idle time and last event, and is killed.
func TestClaudeExecStallsWhenSilent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	r := runFakeClaudeStall(t, fixed("echo $$ > "+pidFile+"\necho '"+initLine+"'\nexec sleep 30\n"))
	err := r.err
	if !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if r.took > 30*r.limit {
		t.Errorf("stall took %s, want about the idle limit %s", r.took, r.limit)
	}
	if !strings.Contains(err.Error(), r.limit.String()) || !strings.Contains(err.Error(), "last event: system") {
		t.Errorf("err = %v, want the idle duration and last event type", err)
	}
	e := retry.Classify("claude", err, false)
	if e.Kind != retry.KindFatal || agent.StatusOf(e) != wire.StatusStalled {
		t.Errorf("kind = %v, status = %q", e.Kind, agent.StatusOf(e))
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid == 0 || proc.Alive(pid) {
		t.Errorf("child pid %d still alive after the stall", pid)
	}
}

// A claude that delivers a successful result and then lingers is killed, but the run succeeds with the answer.
func TestClaudeExecStallsAfterSuccessfulResult(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	r := runFakeClaudeStall(t, fixed("echo $$ > "+pidFile+"\necho '"+resultLine+"'\nexec sleep 30\n"))
	if r.err != nil || r.res.Answer != "done" {
		t.Fatalf("res = %+v, err = %v; want the complete answer and a nil error", r.res, r.err)
	}
	if r.took > 30*r.limit {
		t.Errorf("run took %s, want about the idle limit %s", r.took, r.limit)
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid == 0 || proc.Alive(pid) {
		t.Errorf("lingering child pid %d still alive", pid)
	}
}

// Given claude speaks, then goes silent before any result, Then the run is still an ErrSilent failure.
func TestClaudeExecSilenceBeforeResultStillFails(t *testing.T) {
	text := `{"type":"assistant","session_id":"s","message":{"model":"m","content":[{"type":"text","text":"hi"}]}}`
	err := runFakeClaudeStall(t, fixed("echo '"+text+"'\nexec sleep 30\n")).err
	if !errors.Is(err, agent.ErrSilent) || !strings.Contains(err.Error(), "last event: assistant") {
		t.Fatalf("err = %v, want ErrSilent after the last assistant event", err)
	}
}

func TestClaudeLiveStallsWhenSilent(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	old := claudeIdleLimit
	claudeIdleLimit = 100 * time.Millisecond
	t.Cleanup(func() { claudeIdleLimit = old })
	// the stub stays silent and alive until the test closes its stdin, not for a fixed time
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nread -r prompt\nread -r hold\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*claudeIdleLimit) // scales with the idle window
	defer cancel()
	backend := claudeAgent.NewLive()
	cmd, _, _, err := backend.Build(ctx, agent.RunOptions{Tools: true, Launcher: fakeLauncher{bin}})
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	if err := backend.WriteTurn(stdin, "hello"); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Parse(bufio.NewReader(stdout), wire.Discard, nil)
	if !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("live turn err = %v; want ErrStalled before outer deadline", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("live turn waited for outer deadline: %v", ctx.Err())
	}
}

// Given a tool runs longer than the idle limit, Then the run completes: silence while a tool executes is not a stall.
func TestClaudeExecToolRunDoesNotStall(t *testing.T) {
	r := runFakeClaudeStall(t, func(limit time.Duration) string {
		pause := strconv.FormatFloat((limit * 3 / 2).Seconds(), 'f', 3, 64) // silence past the window, tool still running
		return "echo '" + initLine + "'\necho '" + toolUse + "'\nsleep " + pause + "\necho '" + toolResult + "'\necho '" + resultLine + "'\n"
	})
	if r.err != nil || r.res.Answer != "done" {
		t.Fatalf("res = %+v, err = %v", r.res, r.err)
	}
}

// Given a real capture is replayed, Then the run completes unchanged.
func TestClaudeExecGoldenStreamCompletes(t *testing.T) {
	golden, err := filepath.Abs("testdata/stream.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	res, err := runFakeClaude(t, "cat "+golden+"\n")
	if err != nil || res.SessionID != "uuid_01" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// Given claude never emits anything, Then the window armed at process start trips it.
func TestClaudeExecStallsBeforeFirstEvent(t *testing.T) {
	_, err := runFakeClaudeIdle(t, 100*time.Millisecond, "exec sleep 30\n")
	if !errors.Is(err, agent.ErrStalled) || !strings.Contains(err.Error(), "last event: none") {
		t.Fatalf("err = %v, want ErrStalled with no event seen", err)
	}
}

// Given the stream ends without a result and claude then exits 7, Then the exit status survives: EOF means no kill.
func TestClaudeExecParseErrorAfterEOFKeepsExitStatus(t *testing.T) {
	_, err := runFakeClaude(t, "echo '"+initLine+"'\nexec >&-\nsleep 0.1\nexit 7\n")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !errors.Is(err, errNoResult) {
		t.Fatalf("err = %v; want exit code 7 (not a kill) joined with errNoResult", err)
	}
}

// Given claude emits an error result and keeps running, Then the run fails promptly with that error, not the stall.
func TestClaudeExecMidStreamParseErrorKillsChild(t *testing.T) {
	const idle = 5 * time.Second // far above a loaded child's start-up, so only the kill can end the run early
	start := time.Now()
	errLine := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","session_id":"s"}`
	_, err := runFakeClaudeIdle(t, idle, "echo '"+initLine+"'\necho '"+errLine+"'\nexec sleep 30\n")
	if err == nil || !strings.Contains(err.Error(), "boom") || errors.Is(err, agent.ErrStalled) || time.Since(start) > 2*idle {
		t.Fatalf("took %v, err = %v; want a prompt kill keeping the result error", time.Since(start), err)
	}
}

// Given a top-level tool_use then tool_result, Then each message is decoded once, shared by the stream and idle watch.
func TestParseClaudeEventsDecodesEachMessageOnce(t *testing.T) {
	orig := decodeMessage
	calls := 0
	decodeMessage = func(raw json.RawMessage) anthropic.Message { calls++; return orig(raw) }
	t.Cleanup(func() { decodeMessage = orig })
	w := newIdleWatch(time.Minute, func() {})
	defer w.Stop()
	stream := strings.Join([]string{initLine, toolUse, toolResult, resultLine}, "\n") + "\n"
	if _, err := parseClaudeEvents(bufio.NewReader(strings.NewReader(stream)), false, w, wire.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("decodes = %d, want 2 (one per assistant/user message)", calls)
	}
}
