package doctor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestTranscriptFix(t *testing.T) {
	files, original, young, outside := transcriptFixture(t)
	report := transcriptRun(t, 1, "--secrets", "--since", "1")
	if !strings.Contains(report, "matches 12") {
		t.Fatal("report missed encoded matches")
	}
	transcriptUnchanged(t, append(files, young, outside), original)
	fixed := transcriptRun(t, 0, "--secrets", "--fix", "--since", "1")
	for _, want := range []string{"masked files 3, matches masked 9", "skipped: not idle 1", "skipped: symlink or outside root 1", "skipped: not yet enabled 1"} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("missing count: %s", want)
		}
	}
	transcriptMasked(t, files, original)
	transcriptUnchanged(t, []string{young, outside}, original)
	again := transcriptRun(t, 0, "--secrets", "--fix", "--since", "1")
	if strings.Contains(again, "masked files 1") || strings.Contains(again, "masked files 3") {
		t.Fatal("repeat changed files")
	}
	invalid := filepath.Join(filepath.Dir(young), "invalid.jsonl")
	transcriptWrite(t, invalid, []byte(transcriptFake), 20*time.Minute)
	result := transcriptRun(t, 0, "--secrets", "--fix", invalid)
	if !strings.Contains(result, "skipped: would not stay valid 1") {
		t.Fatal("missing invalid-file skip")
	}
	transcriptUnchanged(t, []string{invalid}, []byte(transcriptFake))
}

const transcriptFake = "FAKE-SECRET-0123456789abcdef"

func transcriptFixture(t *testing.T) ([]string, []byte, string, string) {
	t.Helper()
	cfg := resolvedPath(vaulttest.UseFreshKey(t))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", resolvedPath(t.TempDir()))
	t.Chdir(doctorGitFixture(t, false))
	escaped := "FAKE-SECRET-0123456789ab\"cd\\ef"
	for name, value := range map[string]string{"FAKE_TOKEN": transcriptFake, "FAKE_ESCAPED": escaped} {
		if err := vault.Store(name, value); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(map[string]string{"raw": transcriptFake, "escaped": escaped, "base64": base64.StdEncoding.EncodeToString([]byte(transcriptFake))})
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	var files []string
	for _, rel := range []string{"projects/p/s.jsonl", "projects/p/s/subagents/agent-a.jsonl", "projects/p/s/tool-results/a.txt", "tasks/a.output", "state/harness/dispatch/a.log", "state/harness/results/a.txt", "memory/reports/a.md", "history.jsonl", "state/harness/proxy.log"} {
		path := filepath.Join(cfg, rel)
		transcriptWrite(t, path, b, 20*time.Minute)
		files = append(files, path)
	}
	young := filepath.Join(cfg, "projects/p/young.jsonl")
	transcriptWrite(t, young, b, time.Minute)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	transcriptWrite(t, outside, b, 20*time.Minute)
	if err := os.Symlink(outside, filepath.Join(cfg, "projects/p/link.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".codex/sessions/a.jsonl", ".copilot/session-state/a/events.jsonl"} {
		transcriptWrite(t, filepath.Join(home, rel), b, 20*time.Minute)
	}
	return files, b, young, outside
}

func transcriptWrite(t *testing.T, path string, b []byte, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func transcriptRun(t *testing.T, want int, args ...string) string {
	t.Helper()
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
	if code != want {
		t.Fatalf("exit=%d want=%d stderr-bytes=%d", code, want, len(stderr))
	}
	if strings.Contains(out+stderr, transcriptFake) {
		t.Fatal("value leaked")
	}
	return out
}

func transcriptUnchanged(t *testing.T, files []string, want []byte) {
	t.Helper()
	for _, path := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("file unexpectedly changed")
		}
	}
}

func transcriptMasked(t *testing.T, files []string, original []byte) {
	t.Helper()
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != len(original) || bytes.Count(b, []byte("[redacted]")) != 3 {
			t.Fatal("size or masking assertion failed")
		}
		if bytes.Contains(b, []byte("FAKE-SECRET")) || bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString([]byte(transcriptFake)))) {
			t.Fatal("planted value remains")
		}
		for line := range bytes.SplitSeq(bytes.TrimSpace(b), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatal("invalid JSON after masking")
			}
		}
	}
}
