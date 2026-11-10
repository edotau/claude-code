package memory

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// The dry run lists a quiet interactive session with uncovered edits, skips a headless one and writes no count file.
func TestCatchUpSweepDryRunListsMissedSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(paths.ConfigDir(), "projects", "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const missed, worker = "66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	for id, entry := range map[string]string{missed: "cli", worker: "sdk-cli"} {
		tr := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(tr, []byte(`{"entrypoint":"`+entry+`"}`+"\n"+strings.Repeat(edit, 5)), 0o600); err != nil {
			t.Fatal(err)
		}
		idle(t, tr, 20*time.Minute)
	}
	var out bytes.Buffer
	if rc := CatchUpSweep(SweepOptions{DryRun: true, Stdout: &out, Stderr: io.Discard}); rc != 0 {
		t.Fatalf("dry run exit = %d", rc)
	}
	got := out.String()
	if !strings.Contains(got, "would save "+missed) || !strings.Contains(got, "(5 edits, 0 covered)") ||
		strings.Contains(got, worker) || !strings.Contains(got, "headless (worker/leaf/ping):1") {
		t.Errorf("dry run output:\n%s", got)
	}
	if _, err := os.Stat(countPath(missed)); err == nil {
		t.Error("the dry run wrote a count file")
	}
}
