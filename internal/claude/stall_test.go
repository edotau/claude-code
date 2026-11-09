package claude

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
)

type fakeLauncher struct{ bin string }

func (f fakeLauncher) Claude(context.Context, agent.ClaudeOptions, []string) (agent.ClaudeLaunch, error) {
	return agent.ClaudeLaunch{Binary: f.bin}, nil
}

// runFakeClaude runs claudeExec against a shell script standing in for claude, with the idle limit shortened.
func runFakeClaude(t *testing.T, script string) (wire.Result, error) {
	t.Helper()
	old := claudeIdleLimit
	claudeIdleLimit = 300 * time.Millisecond
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

// Given claude goes silent after system/init, Then the run is a stalled failure naming the idle time and the last
// event, and the child is killed.
func TestClaudeExecStallsWhenSilent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := runFakeClaude(t, "echo $$ > "+pidFile+"\necho '"+initLine+"'\nexec sleep 30\n")
	if !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stall took %s, want about the idle limit", took)
	}
	if !strings.Contains(err.Error(), "300ms") || !strings.Contains(err.Error(), "last event: system") {
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

func TestClaudeExecStallsAfterSuccessfulResult(t *testing.T) {
	res, err := runFakeClaude(t, "echo '"+resultLine+"'\nexec sleep 30\n")
	if !errors.Is(err, agent.ErrStalled) {
		t.Fatalf("err = %v; want ErrStalled after result before EOF", err)
	}
	if res.Answer != "done" || !strings.Contains(err.Error(), "last event: result") {
		t.Fatalf("res = %+v, err = %v; want answer preserved and last event result", res, err)
	}
}

func TestClaudeLiveStallsWhenSilent(t *testing.T) {
	old := claudeIdleLimit
	claudeIdleLimit = 300 * time.Millisecond
	t.Cleanup(func() { claudeIdleLimit = old })
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nread -r prompt\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	defer func() { cancel(); _ = cmd.Wait() }()
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
	res, err := runFakeClaude(t, "echo '"+initLine+"'\necho '"+toolUse+"'\nsleep 1\necho '"+toolResult+"'\necho '"+resultLine+"'\n")
	if err != nil || res.Answer != "done" {
		t.Fatalf("res = %+v, err = %v", res, err)
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
	_, err := runFakeClaude(t, "exec sleep 30\n")
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
	start := time.Now()
	errLine := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","session_id":"s"}`
	_, err := runFakeClaude(t, "echo '"+initLine+"'\necho '"+errLine+"'\nexec sleep 30\n")
	if err == nil || !strings.Contains(err.Error(), "boom") || errors.Is(err, agent.ErrStalled) || time.Since(start) > 5*time.Second {
		t.Fatalf("took %v, err = %v; want a prompt kill keeping the result error", time.Since(start), err)
	}
}
