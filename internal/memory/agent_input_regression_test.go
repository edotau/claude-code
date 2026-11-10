package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCopilotFallbackExcludesFailedEdits(t *testing.T) {
	for _, edits := range []string{"", `,"fileEdits":[]`, `,"fileEdits":[{"path":"/r/rejected.txt"}]`} {
		t.Run("completion"+edits, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			data := `{"type":"tool.execution_start","data":{"toolCallId":"failed","toolName":"create","arguments":{"path":"/r/rejected.txt"}}}` + "\n" +
				`{"type":"tool.execution_complete","data":{"toolCallId":"failed","success":false` + edits + `}}` + "\n" +
				`{"type":"tool.execution_start","data":{"toolCallId":"pending","toolName":"create","arguments":{"path":"/r/pending.txt"}}}` + "\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			got := loadCopilotEvents(copilotEventsFrom(path, "/r"))
			if got.Edits != 1 || fmt.Sprint(got.EditedPaths) != "[/r/pending.txt]" {
				t.Fatalf("fallback edits=%d paths=%v, want only the unfailed pending edit", got.Edits, got.EditedPaths)
			}
		})
	}
}

func TestAgentInputOpenFailurePreservesSynthTranscript(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	events := filepath.Join(t.TempDir(), "events.jsonl")
	data := `{"type":"assistant.message","data":{"content":"Keep this transcript"}}` + "\n"
	if err := os.WriteFile(events, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	path, ok := SynthAgentTranscript(SourceCopilot, "stable-copilot", "/r", events)
	before, err := os.ReadFile(path)
	if !ok || err != nil {
		t.Fatalf("first synth ok=%v read err=%v", ok, err)
	}
	if !bytes.Contains(before, []byte("Keep this transcript")) {
		t.Fatalf("missing initial synthesized text: %s", before)
	}
	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	got, ok := SynthAgentTranscript(SourceCopilot, "stable-copilot", "/r", events)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok || !bytes.Equal(after, before) {
		t.Fatalf("open failure changed result or stable transcript: path=%q ok=%v, transcript=%q", got, ok, after)
	}
}
