package agent

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

const evSecret = "AKIAIOSFODNN7EXAMPLE"

// emitScript is a fake vendor body that drains stdin then prints each NDJSON line on stdout.
func emitScript(lines ...string) string {
	s := "cat > /dev/null\n"
	for _, l := range lines {
		s += "printf '%s\\n' '" + l + "'\n"
	}
	return s
}

// loggedRun runs fn under a WithLog (or WithLogStderr) ctx and returns the closed log's content.
func loggedRun(t *testing.T, stderrOnly bool, fn func(ctx context.Context)) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "d.log")
	sink := NewLogSink(path)
	ctx := WithLog(context.Background(), sink)
	if stderrOnly {
		ctx = WithLogStderr(context.Background(), sink)
	}
	fn(ctx)
	sink.Close()
	return readLog(t, path)
}

// wantLines asserts the log holds exactly the given lines: formatted, redacted, and no raw NDJSON.
func wantLines(t *testing.T, log string, want ...string) {
	t.Helper()
	if strings.Contains(log, evSecret) {
		t.Errorf("log leaks the secret:\n%s", log)
	}
	if strings.Contains(log, `{"`) {
		t.Errorf("log still holds raw JSON events:\n%s", log)
	}
	if got := strings.Split(strings.TrimRight(log, "\n"), "\n"); !equalLines(got, want) {
		t.Errorf("log lines = %q, want %q", got, want)
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLogEventAgyFormatted(t *testing.T) {
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "agy", emitScript(
			`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"line one\nline two AWS_SECRET=`+evSecret+`"}}`,
			`{"event":"result","result":{"status":"SUCCESS","conversation_id":"c1","response":"final-answer"}}`))
		res, err := RunAgy(ctx, AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, &out)
		if err != nil || res.Answer == "" {
			t.Fatalf("RunAgy = (%+v, %v)", res, err)
		}
	})
	if !strings.Contains(out.String(), "line one\nline two") {
		t.Errorf("caller stream altered: %q", out.String())
	}
	wantLines(t, log,
		"▸ assistant: line one line two AWS_SECRET=REDACTED",
		"▸ result: final-answer")
}

func TestLogEventOpenCodeFormatted(t *testing.T) {
	r, err := Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "opencode", emitScript(
			`{"type":"text","sessionID":"ses_A","part":{"text":"hello AWS_SECRET=`+evSecret+`"}}`,
			`{"type":"step_finish","sessionID":"ses_A","part":{"tokens":{"input":3,"output":2}}}`))
		if _, err := r.Run(ctx, Request{Prompt: "p", WorkDir: t.TempDir()}, &out); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if out.String() != "hello AWS_SECRET="+evSecret {
		t.Errorf("caller stream = %q", out.String())
	}
	// step_finish may or may not log a result line; the assistant line is exact and any extra line is formatted.
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if lines[0] != "▸ assistant: hello AWS_SECRET=REDACTED" || len(lines) > 2 {
		t.Errorf("log lines = %q, want the assistant line first", lines)
	}
	if len(lines) == 2 && !strings.HasPrefix(lines[1], "▸ result:") {
		t.Errorf("second log line = %q, want a '▸ result:' line", lines[1])
	}
	if strings.Contains(log, evSecret) || strings.Contains(log, `{"`) {
		t.Errorf("log leaks the secret or raw JSON:\n%s", log)
	}
}

func TestLogEventClaudeFormatted(t *testing.T) {
	r, err := Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "claude", emitScript(
			`{"type":"assistant","message":{"content":[{"type":"text","text":"thinking aloud AWS_SECRET=`+evSecret+`"}]}}`,
			`{"type":"result","subtype":"success","is_error":false,"result":"done-answer","session_id":"s1"}`))
		if _, err := r.Run(ctx, Request{Prompt: "p", WorkDir: t.TempDir()}, &out); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	wantLines(t, log,
		"▸ assistant: thinking aloud AWS_SECRET=REDACTED",
		"▸ result: done-answer")
}

// Under WithLogStderr the caller already logs stdout, so event lines must not be doubled into the log.
func TestLogEventStderrOnlyWritesNoEvents(t *testing.T) {
	log := loggedRun(t, true, func(ctx context.Context) {
		pathstest.FakeVendor(t, "agy", "cat > /dev/null\necho child-err >&2\n"+
			`printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"final-answer"}}'`+"\n")
		if _, err := RunAgy(ctx, AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, io.Discard); err != nil {
			t.Fatalf("RunAgy: %v", err)
		}
	})
	if log != "child-err\n" {
		t.Errorf("stderr-only log = %q, want only the child stderr line", log)
	}
}

// No sink on ctx: the run succeeds and the caller stream is untouched (logEvent is a no-op).
func TestLogEventNoSinkNoop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "agy", emitScript(
		`{"event":"result","result":{"status":"SUCCESS","response":"final-answer"}}`))
	var out strings.Builder
	if _, err := RunAgy(context.Background(), AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "final-answer" {
		t.Errorf("stream = %q", out.String())
	}
}
