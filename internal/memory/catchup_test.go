package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		write(t, tr, `{"type":"user","entrypoint":"`+entry+`","message":{"content":"hello"}}`+"\n"+strings.Repeat(edit, 5))
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

// Given transcripts from each entrypoint, When classified, Then IDE and desktop sessions are people and sdk-* runs are workers.
func TestTranscriptInteractiveByEntrypoint(t *testing.T) {
	dir := t.TempDir()
	for entry, want := range map[string]bool{"cli": true, "claude-vscode": true, "claude-desktop": true, "sdk-cli": false, "sdk-ts": false} {
		path := filepath.Join(dir, entry+".jsonl")
		write(t, path, `{"type":"queue-operation"}`+"\n"+`{"entrypoint":"`+entry+`"}`+"\n"+`{"type":"user","entrypoint":"`+entry+`","message":{"content":"hello"}}`+"\n")
		if got := transcriptInteractive(path); got != want {
			t.Errorf("entrypoint %q interactive = %v, want %v", entry, got, want)
		}
	}
}

// A memory job launched from an IDE session inherits its entrypoint; its own prompt marks it a worker.
func TestTranscriptInteractiveSkipsMemoryJobs(t *testing.T) {
	dir := t.TempDir()
	head := `{"type":"queue-operation"}` + "\n" + `{"type":"attachment","entrypoint":"claude-vscode"}` + "\n"
	for prompt, want := range map[string]bool{
		fmt.Sprintf(updatePromptFormat, "/b", "/h"): false, fmt.Sprintf(dreamPromptFormat, "/b", "/m"): false,
		"fix the login bug": true,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(prompt[:12], " ", "-")+".jsonl")
		write(t, path, head+`{"type":"user","entrypoint":"claude-vscode","message":{"content":"`+prompt+`"}}`+"\n")
		if got := transcriptInteractive(path); got != want {
			t.Errorf("prompt %q interactive = %v, want %v", prompt, got, want)
		}
	}
}

func TestTranscriptInteractiveAdversarial(t *testing.T) {
	const catchupScan = 200 // The former metadata scan limit.
	update := updatePromptOpen + " The bank directory is /b and the shared home bank is /h; write only there. "
	dream := dreamPromptOpen + " The local bank is /b; the memory root /m holds the home bank and every repo store. "
	for _, c := range []struct {
		name, prompt string
		array        bool
		padding      int
		want         bool
	}{
		{"human opening", updatePromptOpen + " Please explain this sentence.", false, 0, true},
		{"array update", update, true, 0, false},
		{"array dream", dream, true, 0, false},
		{"late worker", update, false, catchupScan + 1, false},
		{"no user sdk", "", false, catchupScan + 1, true},
		{"late human", "Please save my changes", false, catchupScan + 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			content, err := json.Marshal(c.prompt)
			if err != nil {
				t.Fatal(err)
			}
			if c.array {
				content = []byte(`[{"type":"text","text":` + string(content) + `}]`)
			}
			path := filepath.Join(t.TempDir(), "session.jsonl")
			record := `{"type":"user","message":{"content":` + string(content) + `}}` + "\n"
			if c.name == "no user sdk" {
				record = ""
			}
			entry := "claude-vscode"
			if c.name == "no user sdk" {
				entry = "sdk-cli"
			}
			write(t, path, `{"entrypoint":"`+entry+`"}`+"\n"+strings.Repeat("{}\n", c.padding)+record)
			if got := transcriptInteractive(path); got != c.want {
				t.Fatalf("interactive=%v, want %v", got, c.want)
			}
		})
	}
}
