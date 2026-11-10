package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAgentInputOpencodeExportFails(t *testing.T) {
	orig := opencodeExport
	opencodeExport = func(string, string) ([]byte, error) { return nil, errors.New("no opencode") }
	t.Cleanup(func() { opencodeExport = orig })
	if path, ok := SynthAgentTranscript(SourceOpencode, "ses_x2", "/r", ""); ok || path != "" {
		t.Fatalf("failed export must fail open, got %q ok=%v", path, ok)
	}
}

// Live copilot 1.0.105 log (trimmed); its rejected create is hand-added: a failed complete.
func TestCopilotEventsFromUsesFileEdits(t *testing.T) {
	got := loadCopilotEvents(copilotEventsFrom("testdata/copilot-live-events.jsonl", "/r"))
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
