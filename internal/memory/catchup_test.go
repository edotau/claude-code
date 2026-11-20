package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// A cached head verdict is reused while size and mtime match, and a transcript that grew is scanned again.
func TestCatchUpHeadCacheKeysOnSizeAndMtime(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const id = "88888888-8888-4888-8888-888888888888"
	tr := filepath.Join(paths.ConfigDir(), "projects", "p", id+".jsonl")
	edit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	write(t, tr, `{"type":"user","entrypoint":"cli","message":{"content":"hello"}}`+"\n"+strings.Repeat(edit, 5))
	idle(t, tr, 20*time.Minute)
	dry := func() string {
		var out bytes.Buffer
		CatchUpSweep(SweepOptions{DryRun: true, Stdout: &out, Stderr: io.Discard})
		return out.String()
	}
	if got := dry(); !strings.Contains(got, "would save "+id) {
		t.Fatalf("interactive session not listed:\n%s", got)
	}
	h := loadHeadCache()
	v, ok := h.Files[tr]
	if !ok || !v.Interactive {
		t.Fatalf("verdict not cached: %+v", h.Files)
	}
	v.Interactive = false
	h.Files[tr], h.dirty = v, true
	h.save(recentTranscripts(time.Now()))
	if got := dry(); strings.Contains(got, id) {
		t.Errorf("an unchanged transcript was rescanned:\n%s", got)
	}
	fi, _ := os.Stat(tr)
	f, err := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(edit)
	f.Close()
	if err := os.Chtimes(tr, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := dry(); !strings.Contains(got, "would save "+id) {
		t.Errorf("a grown transcript kept its stale verdict:\n%s", got)
	}
}

// Given transcripts from each entrypoint, When classified, Then IDE and desktop are people and sdk-* are workers.
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
		fmt.Sprintf(updatePromptFormat, "/b"): false, fmt.Sprintf(dreamPromptFormat, "/b"): false,
		fmt.Sprintf(legacyUpdatePromptFormat, "/b", "/h"): false, fmt.Sprintf(legacyDreamPromptFormat, "/b", "/m"): false,
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
	update := fmt.Sprintf(updatePromptFormat, "/b")
	dream := fmt.Sprintf(dreamPromptFormat, "/b")
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

// Row 5: a sweep error names the live `memory update --catchup` verb, not the deleted `memory catchup`.
func TestCatchUpSweepErrorNamesLiveVerb(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	write(t, paths.MemoryStateDir(), "a file where the state dir should be")
	var errOut bytes.Buffer
	if rc := CatchUpSweep(SweepOptions{Stdout: io.Discard, Stderr: &errOut}); rc != 1 {
		t.Fatalf("exit = %d, want 1", rc)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "memory update --catchup: ") {
		t.Errorf("stderr = %q", got)
	}
}

// Row 16: a failed catch-up retries once more after 24 h, then never again (two retries in all).
func TestRunSweepRetriesFailedCatchupOnceMore(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const id = "88888888-8888-4888-8888-888888888888"
	tr := filepath.Join(paths.ConfigDir(), "projects", "p", id+".jsonl")
	write(t, tr, `{"type":"user","entrypoint":"cli","message":{"content":"hello"}}`+"\n")
	idle(t, tr, 72*time.Hour)
	now := time.Now()
	if err := writeStamp(stampPath(id), now.Add(-48*time.Hour), "catchup", 0); err != nil {
		t.Fatal(err)
	}
	fail := func(at time.Time) {
		if err := writeUpdateResult(id, UpdateResult{Status: updateFailed, Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	fail(now.Add(-47 * time.Hour))
	calls := 0
	orig := runHistoricalUpdate
	t.Cleanup(func() { runHistoricalUpdate = orig })
	runHistoricalUpdate = func(context.Context, string, sweepCand, io.Writer) error { calls++; fail(time.Now()); return nil }
	runSweep("", now, io.Discard)
	runSweep("", now.Add(48*time.Hour), io.Discard)
	if calls != 1 {
		t.Errorf("retries after the first = %d, want 1", calls)
	}
}

// A second retry whose child never writes a receipt must not repeat on every later sweep.
func TestRunSweepStopsAfterSecondRetryWithoutReceipt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const id = "99999999-9999-4999-8999-999999999999"
	tr := filepath.Join(paths.ConfigDir(), "projects", "p", id+".jsonl")
	write(t, tr, `{"type":"user","entrypoint":"cli","message":{"content":"hello"}}`+"\n")
	idle(t, tr, 72*time.Hour)
	now := time.Now()
	if err := writeStamp(stampPath(id), now.Add(-48*time.Hour), "catchup", 0); err != nil {
		t.Fatal(err)
	}
	if err := writeUpdateResult(id, UpdateResult{Status: updateFailed, Time: now.Add(-47 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	orig := runHistoricalUpdate
	t.Cleanup(func() { runHistoricalUpdate = orig })
	runHistoricalUpdate = func(context.Context, string, sweepCand, io.Writer) error { calls++; return errors.New("spawn failed") }
	for i := range 4 {
		runSweep("", now.Add(time.Duration(i)*time.Hour), io.Discard)
	}
	if calls != 1 {
		t.Errorf("second retries with no receipt = %d, want 1", calls)
	}
	if st, ok := readStamp(stampPath(id)); !ok || st.attempts != 2 {
		t.Errorf("stamp after the second retry = %+v ok=%v, want attempts 2", st, ok)
	}
}

// A catchup stamp with a fourth field round-trips; a three-field one reads as attempts 0.
func TestReadStampAttemptsField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stamp-x")
	at := time.Now().Truncate(time.Second)
	if err := writeStampAttempts(path, at, "catchup", 7, 2); err != nil {
		t.Fatal(err)
	}
	if st, ok := readStamp(path); !ok || st.attempts != 2 || st.edits != 7 || st.state != "catchup" {
		t.Errorf("four-field stamp = %+v ok=%v", st, ok)
	}
	if err := writeStamp(path, at, "bg", 3); err != nil {
		t.Fatal(err)
	}
	if st, ok := readStamp(path); !ok || st.attempts != 0 {
		t.Errorf("three-field stamp = %+v ok=%v", st, ok)
	}
}
