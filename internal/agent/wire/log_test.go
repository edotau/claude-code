package wire

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/textutil"
)

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
	return pathstest.ReadFile(t, path)
}

// A job carrier must not tee plain-text vendor stdout via teeWriter, a plain WithLog must; both feed stderr/events.
func TestLogEventJobModeNoRawStdoutTee(t *testing.T) {
	for _, tc := range []struct {
		name    string
		job     bool
		wantRaw bool
	}{{"job carrier", true, false}, {"plain WithLog", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var caller strings.Builder
			log := loggedRun(t, tc.job, func(ctx context.Context) {
				if StderrLogFrom(ctx) == nil {
					t.Fatal("stderrLogFrom lost the sink")
				}
				w := TeeWriter(ctx, &caller)
				_, _ = io.WriteString(w, "raw-stdout-answer\n")
				LogEvent(ctx, "result", "ev-line")
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

// TestLogSinkModeAppendRedact: the log is 0600, a second sink appends, and a secret split across Writes is redacted.
func TestLogSinkModeAppendRedact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s1 := NewLogSink(path)
	_, _ = s1.Stream().Write([]byte("first\n"))
	s1.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("log mode = %o, want 600", mode)
	}

	token := "ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	line := "key=sk-" + token + "\n"
	if textutil.RedactSecrets(line) == line {
		t.Fatalf("textutil.RedactSecrets(%q) left it unchanged; test has no teeth", line)
	}
	s2 := NewLogSink(path)
	w := s2.Stream()
	_, _ = w.Write([]byte("key=sk-"))
	_, _ = w.Write([]byte(token))
	_, _ = w.Write([]byte("\n"))
	s2.Close()

	got := pathstest.ReadFile(t, path)
	if strings.Contains(got, token) {
		t.Errorf("log leaks the raw token: %q", got)
	}
	if want := "first\n" + textutil.RedactSecrets(line); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// TestLogSinkPartialLineFlushedOnClose: an unterminated last line waits for Close, which adds the newline.
func TestLogSinkPartialLineFlushedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	_, _ = s.Stream().Write([]byte("whole\npartial"))
	if got := pathstest.ReadFile(t, path); got != "whole\n" {
		t.Errorf("log before Close = %q, want %q", got, "whole\n")
	}
	s.Close()
	if got := pathstest.ReadFile(t, path); got != "whole\npartial\n" {
		t.Errorf("log after Close = %q, want %q", got, "whole\npartial\n")
	}
}

// TestLogSinkLazyCreate: NewLogSink alone creates no file; the first Stream does.
func TestLogSinkLazyCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Stat before Stream err = %v, want not-exist", err)
	}
	s.Stream()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Stat after Stream: %v", err)
	}
	s.Close()
}

// TestLogSinkConcurrentStreamsNoInterleave: two streams written concurrently keep every log line whole.
func TestLogSinkConcurrentStreamsNoInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	const n = 500
	want := map[string]bool{}
	var wg sync.WaitGroup
	for _, name := range []string{"stdout", "stderr"} {
		w := s.Stream()
		for i := range n {
			want[fmt.Sprintf("%s line %d %s", name, i, strings.Repeat(name[3:], 20))] = true
		}
		wg.Go(func() {
			for i := range n {
				l := fmt.Sprintf("%s line %d %s\n", name, i, strings.Repeat(name[3:], 20))
				_, _ = w.Write([]byte(l[:len(l)/2])) // split each line across two Writes
				_, _ = w.Write([]byte(l[len(l)/2:]))
			}
		})
	}
	wg.Wait()
	s.Close()

	lines := strings.Split(strings.TrimSuffix(pathstest.ReadFile(t, path), "\n"), "\n")
	if len(lines) != 2*n {
		t.Fatalf("log has %d lines, want %d", len(lines), 2*n)
	}
	for _, l := range lines {
		if !want[l] {
			t.Fatalf("log line %q is not a whole expected line", l)
		}
		delete(want, l)
	}
}
