package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// readLog reads path, failing the test on error.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
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
	if shapes.Redact(line) == line {
		t.Fatalf("shapes.Redact(%q) left it unchanged; test has no teeth", line)
	}
	s2 := NewLogSink(path)
	w := s2.Stream()
	_, _ = w.Write([]byte("key=sk-"))
	_, _ = w.Write([]byte(token))
	_, _ = w.Write([]byte("\n"))
	s2.Close()

	got := readLog(t, path)
	if strings.Contains(got, token) {
		t.Errorf("log leaks the raw token: %q", got)
	}
	if want := "first\n" + shapes.Redact(line); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// TestLogSinkStdoutByteIdentical: a sink must not change what the caller's stdout receives.
func TestLogSinkStdoutByteIdentical(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\nprintf 'alpha\\nbeta\\n'\necho oops >&2\nprintf 'gamma\\nno-newline'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context) []byte {
		t.Helper()
		var buf bytes.Buffer
		cmd, _ := childCmd(ctx, bin, nil, nil, "", "", nil, true)
		cmd.Stdout = teeWriter(ctx, &buf)
		if err := cmd.Run(); err != nil {
			t.Fatalf("run fake: %v", err)
		}
		return buf.Bytes()
	}
	plain := run(context.Background())
	logPath := filepath.Join(dir, "job.log")
	sink := NewLogSink(logPath)
	teed := run(WithLog(context.Background(), sink))
	sink.Close()

	if !bytes.Equal(plain, teed) {
		t.Errorf("stdout differs with a sink:\nplain %q\nteed  %q", plain, teed)
	}
	if want := "alpha\nbeta\ngamma\nno-newline"; string(plain) != want {
		t.Errorf("stdout = %q, want %q", plain, want)
	}
	log := readLog(t, logPath)
	for _, l := range []string{"alpha\n", "beta\n", "gamma\n", "no-newline\n", "oops\n"} {
		if !strings.Contains(log, l) {
			t.Errorf("log %q missing %q", log, l)
		}
	}
}

// TestLogSinkPartialLineFlushedOnClose: an unterminated last line waits for Close, which adds the newline.
func TestLogSinkPartialLineFlushedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	s := NewLogSink(path)
	_, _ = s.Stream().Write([]byte("whole\npartial"))
	if got := readLog(t, path); got != "whole\n" {
		t.Errorf("log before Close = %q, want %q", got, "whole\n")
	}
	s.Close()
	if got := readLog(t, path); got != "whole\npartial\n" {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n {
				l := fmt.Sprintf("%s line %d %s\n", name, i, strings.Repeat(name[3:], 20))
				_, _ = w.Write([]byte(l[:len(l)/2])) // split each line across two Writes
				_, _ = w.Write([]byte(l[len(l)/2:]))
			}
		}()
	}
	wg.Wait()
	s.Close()

	lines := strings.Split(strings.TrimSuffix(readLog(t, path), "\n"), "\n")
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
