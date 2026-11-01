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
		if _, err := r.Run(ctx, RunOptions{Prompt: "p", WorkDir: t.TempDir()}, NewTextSink(&out)); err != nil {
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
		if _, err := r.Run(ctx, RunOptions{Prompt: "p", WorkDir: t.TempDir(), Launcher: testLauncher{}}, NewTextSink(&out)); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	wantLines(t, log,
		"▸ assistant: thinking aloud AWS_SECRET=REDACTED",
		"▸ result: done-answer")
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
				w := TeeWriter(ctx, &caller)
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
