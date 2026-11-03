package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

type fakeLauncher struct{ bin string }

func (f fakeLauncher) Claude(context.Context, agent.ClaudeOptions, []string) (agent.ClaudeLaunch, error) {
	return agent.ClaudeLaunch{Binary: f.bin}, nil
}

// runFakeClaude runs claudeExec against a shell script standing in for claude, with the idle limit shortened.
func runFakeClaude(t *testing.T, script string) (agent.Result, error) {
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
	return claudeAgent.Run(ctx, agent.RunOptions{Prompt: "x", Tools: true, Launcher: fakeLauncher{bin}}, agent.Discard)
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
	e := agent.Classify("claude", err, false)
	if e.Kind != agent.KindFatal || agent.StatusOf(e) != agent.StatusStalled {
		t.Errorf("kind = %v, status = %q", e.Kind, agent.StatusOf(e))
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Errorf("child pid %d still alive after the stall", pid)
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
