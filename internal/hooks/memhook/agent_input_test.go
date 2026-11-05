package memhook

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func agentIn(t *testing.T, payload string) *core.Input {
	t.Helper()
	return core.ParseInput(strings.NewReader(payload))
}

func TestAgentInputOpencodeExportCounts(t *testing.T) {
	data, err := os.ReadFile("testdata/opencode-export.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotID string
	orig := opencodeExport
	opencodeExport = func(id, _ string) ([]byte, error) { gotID = id; return data, nil }
	t.Cleanup(func() { opencodeExport = orig })
	in := agentIn(t, `{"session_id":"ses_x1","cwd":"/r","harness_agent":"opencode"}`)
	out := AgentInput(in)
	if gotID != "ses_x1" || !updatable(out) {
		t.Fatalf("export id %q, updatable %v", gotID, updatable(out))
	}
	if n := countEdits(out.TranscriptPath, countPath("ses_x1")); n != 2 {
		t.Fatalf("work = %d, want the export's 2 edits", n)
	}
}

func TestAgentInputOpencodeExportFails(t *testing.T) {
	orig := opencodeExport
	opencodeExport = func(string, string) ([]byte, error) { return nil, errors.New("no opencode") }
	t.Cleanup(func() { opencodeExport = orig })
	in := agentIn(t, `{"session_id":"ses_x2","cwd":"/r","harness_agent":"opencode"}`)
	if out := AgentInput(in); out.TranscriptPath != "" {
		t.Fatalf("failed export must leave the input unchanged, got %q", out.TranscriptPath)
	}
}

func TestAgentInputCopilotEventsFile(t *testing.T) {
	events, err := filepath.Abs("testdata/copilot-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	in := agentIn(t, `{"sessionId":"cop-1","cwd":"/r","transcriptPath":"`+events+`"}`)
	out := AgentInput(in)
	if out.TranscriptPath == events {
		t.Fatal("copilot events were not converted")
	}
	if n := countEdits(out.TranscriptPath, countPath("cop-1")); n != 1 {
		t.Fatalf("work = %d, want the apply_patch's 1 edit", n)
	}
	b, _ := os.ReadFile(out.TranscriptPath)
	if !strings.Contains(string(b), `/r/a.txt`) || !strings.Contains(string(b), "Created") {
		t.Fatalf("synthesized transcript lacks the patched path or final text: %s", b)
	}
}

func TestAgentInputClaudePassesThrough(t *testing.T) {
	in := agentIn(t, `{"session_id":"s9","cwd":"/r","transcript_path":"/t.jsonl"}`)
	if out := AgentInput(in); out != in {
		t.Fatal("a Claude-shaped input must come back untouched")
	}
}

// Live copilot 1.0.105 log (trimmed); its rejected create is hand-added: a failed complete.
func TestCopilotEventsFromUsesFileEdits(t *testing.T) {
	got := loadCopilotEvents("cop-live", copilotEventsFrom("testdata/copilot-live-events.jsonl", "/r"))
	if got.Edits != 2 || len(got.EditedPaths) != 2 {
		t.Fatalf("edits = %d paths %v, want the 2 successful fileEdits", got.Edits, got.EditedPaths)
	}
	if !strings.HasSuffix(got.EditedPaths[0], "/a.go") || !strings.HasSuffix(got.EditedPaths[1], "/b.txt") {
		t.Fatalf("paths = %v, want a.go and b.txt", got.EditedPaths)
	}
	if !strings.HasPrefix(got.FinalText, "I created") {
		t.Fatalf("final text = %q", got.FinalText)
	}
}

func TestOpencodeExportRejectsOutputOverCap(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nhead -c " + strconv.Itoa(exportMaxBytes+1<<20) + " /dev/zero\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if data, err := opencodeExport("ses_1", dir); err == nil {
		t.Fatalf("export over %d bytes accepted (%d bytes)", exportMaxBytes, len(data))
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\nprintf '{}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := opencodeExport("ses_1", dir); err != nil || string(data) != "{}" {
		t.Fatalf("small export = %q, %v; want {}", data, err)
	}
}
