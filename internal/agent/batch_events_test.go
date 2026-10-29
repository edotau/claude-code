package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every --batch task goes through Dispatch: it leaves an <id>.events.jsonl running start to result.
func TestBatchTasksWriteEventFiles(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "one", "agent": "claude", "prompt": "p1", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "two", "agent": "claude", "prompt": "p2", "workdir": t.TempDir(), "tools": true}),
	)
	if code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	files, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "dispatch", "*.events.jsonl"))
	if len(files) != 2 {
		t.Fatalf("event files = %v, want 2 (one per task)", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) < 2 || !strings.Contains(lines[0], `"type":"start"`) || !strings.Contains(lines[len(lines)-1], `"type":"result"`) {
			t.Errorf("%s: want start...result, got\n%s", f, b)
		}
	}
}
