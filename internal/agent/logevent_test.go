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
	var b strings.Builder
	b.WriteString("cat > /dev/null\n")
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + l + "'\n")
	}
	return b.String()
}

// loggedRun runs fn under a WithLog (or, for a job run, WithLogEvents) ctx and returns the closed log's content.
func loggedRun(t *testing.T, jobMode bool, fn func(ctx context.Context)) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "d.log")
	sink := NewLogSink(path)
	ctx := WithLog(context.Background(), sink)
	if jobMode {
		ctx = WithLogEvents(context.Background(), sink)
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
		res, err := RunAgy(ctx, AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, NewTextSink(&out))
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
	t.Setenv("OPENCODE_API_KEY", "test-key")
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "opencode", emitScript(
			`{"type":"text","sessionID":"ses_A","part":{"text":"hello AWS_SECRET=`+evSecret+`"}}`,
			`{"type":"step_finish","sessionID":"ses_A","part":{"tokens":{"input":3,"output":2}}}`))
		if _, err := r.Run(ctx, Request{Prompt: "p", WorkDir: t.TempDir()}, NewTextSink(&out)); err != nil {
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
		if _, err := r.Run(ctx, Request{Prompt: "p", WorkDir: t.TempDir()}, NewTextSink(&out)); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	wantLines(t, log,
		"▸ assistant: thinking aloud AWS_SECRET=REDACTED",
		"▸ result: done-answer")
}

// Under WithLogEvents (a job run) the log holds child stderr plus the formatted event lines, redacted, with
// no raw NDJSON; the final answer is the caller's to write, so it appears only as the one result line.
func TestLogEventJobModeWritesEventsAndStderr(t *testing.T) {
	log := loggedRun(t, true, func(ctx context.Context) {
		pathstest.FakeVendor(t, "agy", "cat > /dev/null\necho child-err >&2\n"+
			`printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi AWS_SECRET=`+evSecret+`"}}'`+"\n"+
			`printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"final-answer"}}'`+"\n")
		if _, err := RunAgy(ctx, AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, Discard); err != nil {
			t.Fatalf("RunAgy: %v", err)
		}
	})
	// stderr and stdout are separate pipes, so the stderr line may land anywhere between the event lines.
	if strings.Count(log, "child-err\n") != 1 {
		t.Errorf("job log must hold the child stderr line once: %q", log)
	}
	wantLines(t, strings.Replace(log, "child-err\n", "", 1), "▸ assistant: hi AWS_SECRET=REDACTED", "▸ result: final-answer")
}

// The plain-text vendor path (codex etc.) tees stdout via teeWriter: a job carrier must not tee it, a plain
// WithLog still must, and both keep feeding stderr/event lines.
func TestLogEventJobModeNoRawStdoutTee(t *testing.T) {
	for _, tc := range []struct {
		name    string
		job     bool
		wantRaw bool
	}{{"job carrier", true, false}, {"plain WithLog", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var caller strings.Builder
			log := loggedRun(t, tc.job, func(ctx context.Context) {
				if stderrLogFrom(ctx) == nil {
					t.Fatal("stderrLogFrom lost the sink")
				}
				w := teeWriter(ctx, &caller)
				_, _ = io.WriteString(w, "raw-stdout-answer\n")
				logEvent(ctx, "result", "ev-line")
			})
			if caller.String() != "raw-stdout-answer\n" {
				t.Errorf("caller stream = %q", caller.String())
			}
			if got := strings.Contains(log, "raw-stdout-answer"); got != tc.wantRaw {
				t.Errorf("raw stdout in log = %v, want %v:\n%s", got, tc.wantRaw, log)
			}
			if !strings.Contains(log, "▸ result: ev-line") {
				t.Errorf("event line missing:\n%s", log)
			}
		})
	}
}

// No sink on ctx: the run succeeds and the caller stream is untouched (logEvent is a no-op).
func TestLogEventNoSinkNoop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "agy", emitScript(
		`{"event":"result","result":{"status":"SUCCESS","response":"final-answer"}}`))
	var out strings.Builder
	if _, err := RunAgy(context.Background(), AgyRequest{Cwd: t.TempDir(), Prompt: "t"}, NewTextSink(&out)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "final-answer" {
		t.Errorf("stream = %q", out.String())
	}
}
