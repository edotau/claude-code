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

// fakeOpencode puts an `opencode` script running body first on PATH and returns a directory to use as cwd.
func fakeOpencode(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestAgentInputOpencodeExportCounts(t *testing.T) {
	data, err := filepath.Abs("testdata/opencode-export.json")
	if err != nil {
		t.Fatal(err)
	}
	cwd := fakeOpencode(t, `[ "$*" = "export ses_x1" ] || exit 1; cat '`+data+`'`)
	path, ok := SynthAgentTranscript(SourceOpencode, "ses_x1", cwd, "")
	if !ok || !Updatable("ses_x1", path) {
		t.Fatalf("export for ses_x1 not converted: path %q ok %v, updatable %v", path, ok, Updatable("ses_x1", path))
	}
	if n := CountEdits("ses_x1", path); n != 2 {
		t.Fatalf("work = %d, want the export's 2 edits", n)
	}
}

func TestAgentInputCopilotEventsFile(t *testing.T) {
	events, err := filepath.Abs("testdata/copilot-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path, ok := SynthAgentTranscript(SourceCopilot, "cop-1", "/r", events)
	if !ok || path == events {
		t.Fatal("copilot events were not converted")
	}
	if n := CountEdits("cop-1", path); n != 1 {
		t.Fatalf("work = %d, want the apply_patch's 1 edit", n)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `/r/a.txt`) || !strings.Contains(string(b), "Created") {
		t.Fatalf("synthesized transcript lacks the patched path or final text: %s", b)
	}
}
