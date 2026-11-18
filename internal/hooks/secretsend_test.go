package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func sessionEndInput(path string) *core.Input {
	return core.ParseInput(strings.NewReader(`{"hook_event_name":"SessionEnd","reason":"clear","transcript_path":"` + path + `"}`))
}

// A transcript under projects/ starts the detached pass; a path outside it or through a link starts nothing.
func TestSecretsEndSpawnsOnlyForRegularFilesUnderProjects(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := filepath.Join(cfg, "projects", "p")
	outside := filepath.Join(cfg, "elsewhere")
	for _, d := range []string{proj, outside} {
		_ = os.MkdirAll(d, 0o755)
	}
	good, stray := filepath.Join(proj, "s.jsonl"), filepath.Join(outside, "s.jsonl")
	_ = os.WriteFile(good, []byte("{}\n"), 0o600)
	_ = os.WriteFile(stray, []byte("{}\n"), 0o600)
	fileLink, dirLink := filepath.Join(proj, "link.jsonl"), filepath.Join(cfg, "projects", "dirlink")
	if os.Symlink(good, fileLink) != nil || os.Symlink(outside, dirLink) != nil {
		t.Skip("symlinks unavailable")
	}
	var spawned [][]string
	defer func(prev func([]string) bool) { spawnSecretsJob = prev }(spawnSecretsJob)
	spawnSecretsJob = func(a []string) bool { spawned = append(spawned, a); return true }

	if r := SecretsEnd(sessionEndInput(good)); r.Stderr != "" || len(spawned) != 1 || !slices.Contains(spawned[0], good) {
		t.Fatalf("good transcript: result %+v, spawned %v", r, spawned)
	}
	for _, p := range []string{stray, fileLink, filepath.Join(dirLink, "s.jsonl"), filepath.Join(proj, "missing.jsonl")} {
		if r := SecretsEnd(sessionEndInput(p)); r.Stderr == "" || len(spawned) != 1 {
			t.Errorf("%s: result %+v, spawned %d, want a refusal and no spawn", filepath.Base(p), r, len(spawned))
		}
	}
}

// The pass masks the transcript and its subagent file at the same size, and leaves a file still being written alone.
func TestSecretsJobMasksIdleFilesAndSkipsBusyOnes(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	redact.StubValues(t, []string{fakeSecret})
	proj := filepath.Join(cfg, "projects", "p")
	_ = os.MkdirAll(filepath.Join(proj, "s", "subagents"), 0o755)
	line := []byte(`{"text":"` + fakeSecret + `"}` + "\n")
	main, sub, busy := filepath.Join(proj, "s.jsonl"), filepath.Join(proj, "s", "subagents", "agent-1.jsonl"), filepath.Join(proj, "busy.jsonl")
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{main, sub} {
		_ = os.WriteFile(f, line, 0o600)
		_ = os.Chtimes(f, old, old)
	}
	_ = os.WriteFile(busy, line, 0o600)
	prevSleep := sleepFn
	sleepFn = func(time.Duration) {}
	defer func() { sleepFn = prevSleep }()

	if code := runSecretsJob("SessionEnd", []string{main, "clear"}); code != 0 {
		t.Fatalf("job exit %d", code)
	}
	runSecretsJob("SessionEnd", []string{busy, "other"})
	for _, f := range []string{main, sub} {
		b, _ := os.ReadFile(f)
		if len(b) != len(line) || strings.Contains(string(b), fakeSecret) {
			t.Errorf("%s: size %d, want %d with the value masked", filepath.Base(f), len(b), len(line))
		}
	}
	if b, _ := os.ReadFile(busy); !strings.Contains(string(b), fakeSecret) {
		t.Error("a file modified just now was masked")
	}
	if log, _ := os.ReadFile(filepath.Join(cfg, "state", "harness", "hooks", "secrets-end.log")); !strings.Contains(string(log), "masked=2 hits=2") {
		t.Errorf("log %q lacks the counts", log)
	}
}

// A session directory swapped for a link during the idle wait never redirects masking outside projects/.
func TestSecretsJobRefusesDirectorySwappedDuringWait(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	redact.StubValues(t, []string{fakeSecret})
	proj, outside := filepath.Join(cfg, "projects", "p"), filepath.Join(cfg, "outside")
	_ = os.MkdirAll(proj, 0o755)
	_ = os.MkdirAll(outside, 0o755)
	line := []byte(`{"text":"` + fakeSecret + `"}` + "\n")
	inside, stray := filepath.Join(proj, "s.jsonl"), filepath.Join(outside, "s.jsonl")
	_ = os.WriteFile(inside, line, 0o600)
	_ = os.WriteFile(stray, line, 0o600)
	recent, old := time.Now().Add(-30*time.Minute), time.Now().Add(-2*time.Hour)
	_ = os.Chtimes(inside, recent, recent)
	_ = os.Chtimes(stray, old, old)
	defer func(idle time.Duration, sleep func(time.Duration)) { secretsIdle, sleepFn = idle, sleep }(secretsIdle, sleepFn)
	secretsIdle = time.Hour
	swapped := false
	sleepFn = func(time.Duration) {
		if !swapped {
			swapped = os.Rename(proj, proj+".bak") == nil && os.Symlink(outside, proj) == nil
		}
	}
	code := runSecretsJob("SessionEnd", []string{inside, "clear"})
	if !swapped {
		t.Skip("symlinks unavailable")
	}
	if code != 0 {
		t.Fatalf("job exit %d", code)
	}
	if b, _ := os.ReadFile(stray); !bytes.Equal(b, line) {
		t.Fatalf("a file outside projects/ was rewritten: %q", b)
	}
}
