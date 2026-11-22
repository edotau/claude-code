package codex

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// A completed answer survives a later typed-decode failure; a garbage line before it stays tolerated.
func TestParseCodexStreamDecodeErrorKeepsAnswer(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"th-1"}
not json
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":123}`
	res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "done" || res.SessionID != "th-1" {
		t.Fatalf("res = %+v, err = %v; want the decode error with answer and session kept", res, err)
	}
}

// cached_input_tokens above input_tokens must not push the summed input usage negative.
func TestParseCodexStreamCachedInputNeverNegative(t *testing.T) {
	stream := `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":5}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":30,"output_tokens":2}}`
	res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	want := wire.Usage{InputTokens: 60, CacheReadTokens: 70, OutputTokens: 7}
	if err != nil || res.Usage != want {
		t.Fatalf("usage = %+v, err = %v; want %+v", res.Usage, err, want)
	}
}

func TestParseCodexStreamTopLevelErrors(t *testing.T) {
	const retry = `{"type":"error","will_retry":true,"message":"temporary outage"}`
	const done = "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"42"}}` + "\n" + `{"type":"turn.completed"}`
	cases := []struct{ name, events, answer, failure string }{
		{"recovered", retry + done, "42", ""},
		{"reconnected", `{"type":"error","message":"Reconnecting..."}` + done, "42", ""},
		{"fatal", `{"type":"error","message":"permanent failure"}`, "", "permanent failure"},
		{"truncated", retry, "", "temporary outage"},
		{"turn failed", `{"type":"turn.failed","will_retry":true,"error":{"message":"turn failure"}}` + done, "42", "turn failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := `{"type":"thread.started","thread_id":"probe"}` + "\n" + tc.events
			res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
			failure := ""
			if err != nil {
				failure = err.Error()
			}
			t.Logf("answer=%q error=%v", res.Answer, err)
			if res.Answer != tc.answer || failure != tc.failure {
				t.Fatalf("answer=%q error=%s; want answer=%q error=%s", res.Answer, failure, tc.answer, tc.failure)
			}
		})
	}
}

const (
	evThread = `{"type":"thread.started","thread_id":"t"}`
	evMsg    = `{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"hi"}}`
	evDone   = `{"type":"turn.completed","usage":{}}`
	evRetry  = `{"type":"error","will_retry":true,"message":"reconnecting"}`
	evToolA  = `{"type":"item.started","item":{"id":"c","type":"command_execution","command":"make"}}`
	evToolZ  = `{"type":"item.completed","item":{"id":"c","type":"command_execution","status":"completed","exit_code":0}}`
)

func emit(evs ...string) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString("printf '%s\\n' '" + e + "'\n")
	}
	return b.String()
}

// runIdleScript runs a fake codex shell script under a 150ms idle limit and returns the stream's error.
func runIdleScript(t *testing.T, script string) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	old := agent.IdleLimit
	agent.IdleLimit = 150 * time.Millisecond
	t.Cleanup(func() { agent.IdleLimit = old })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd, tail := agent.ChildCmd(ctx, "/bin/sh", []string{"-c", script}, nil, "", "", nil, true)
	sink := &denialSink{next: wire.Discard, sandbox: "read-only", commands: map[string]string{}}
	_, err := sink.runStream(ctx, "codex", cmd, tail, wire.Result{})
	return err
}

// A codex stream silent after its first item is cut with ErrSilent and its child dies; steady events, a long tool call
// and a retry loop behave as the claude watch does.
func TestCodexIdleWatch(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	err := runIdleScript(t, emit(evThread, evMsg)+"echo $$ > "+pidFile+"\nexec sleep 30\n")
	if !errors.Is(err, agent.ErrSilent) {
		t.Fatalf("silent stream: err = %v, want ErrSilent", err)
	}
	raw, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if p, _ := os.FindProcess(pid); pid == 0 || p.Signal(syscall.Signal(0)) == nil {
		t.Fatalf("child %d still alive after the idle cutoff", pid)
	}
	steady := emit(evThread, evMsg) + strings.Repeat("sleep 0.05\n"+emit(evMsg), 8) + emit(evDone)
	if err := runIdleScript(t, steady); err != nil {
		t.Fatalf("steady events: %v", err)
	}
	tool := emit(evThread, evToolA) + "sleep 0.4\n" + emit(evToolZ, evMsg, evDone)
	if err := runIdleScript(t, tool); err != nil {
		t.Fatalf("long tool call: %v", err)
	}
	loop := emit(evThread, evMsg) + strings.Repeat("sleep 0.05\n"+emit(evRetry), 8) + emit(evDone)
	if err := runIdleScript(t, loop); !errors.Is(err, agent.ErrSilent) {
		t.Fatalf("retry loop: err = %v, want ErrSilent", err)
	}
}

// A completed file_change item's paths land in Result.Written; a failed one's do not.
func TestParseCodexStreamCollectsFileChanges(t *testing.T) {
	stream := `{"type":"item.completed","item":{"type":"file_change","status":"completed","changes":[{"path":"/m/r.md","kind":"add"}]}}
{"type":"item.completed","item":{"type":"file_change","status":"failed","changes":[{"path":"/m/x.md","kind":"add"}]}}`
	res, _ := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if len(res.Written) != 1 || res.Written[0] != "/m/r.md" {
		t.Fatalf("Written = %v, want [/m/r.md]", res.Written)
	}
}
