package agent

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// A bridge dispatch on the agy backend leaves a 0600 <id>.log (record.Log) holding the redacted agy output.
func TestBridgeLogCapturesRedactedAgyOutput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	script := "cat > /dev/null\n" +
		"echo 'agy-stderr-line' >&2\n" +
		"echo 'export AWS_SECRET=AKIAIOSFODNN7EXAMPLE' >&2\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"bridge-answer","num_turns":1}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)

	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil || res.Answer != "bridge-answer" {
		t.Fatalf("Run = (%q, %v), want (bridge-answer, nil)", res.Answer, err)
	}
	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	logPath := recs[0].Log
	if logPath == "" {
		t.Fatal("record.Log is empty: the bridge dispatch did not record a log path")
	}
	if logPath != recs[0].LogPath() {
		t.Errorf("record.Log = %q, want %q", logPath, recs[0].LogPath())
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("log file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 0600", perm)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"agy-stderr-line", "bridge-answer", "AWS_SECRET=REDACTED"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("log leaks an unredacted secret:\n%s", got)
	}
	if n := strings.Count(got, "agy-stderr-line"); n != 1 {
		t.Errorf("stderr line appears %d times, want exactly once:\n%s", n, got)
	}
}
