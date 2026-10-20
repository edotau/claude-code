package agenttree

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildAgentLine makes one assistant transcript record carrying a single text content block;
// eventsFromLine only reads type=="assistant" records whose content holds tool_use/text blocks.
func buildAgentLine(text string) string {
	return `{"type":"assistant","timestamp":"2026-10-18T03:00:00Z","message":{"content":[{"type":"text","text":"` +
		text + `"}]}}`
}

// followFixture lays down a minimal unfinished subagent (so Follow never reaches followDone) with n
// text-event lines in its transcript, returning the ViewOptions to Follow it and the transcript path
// so the test can shrink it mid-poll.
func followFixture(t *testing.T, n int) (ViewOptions, string) {
	t.Helper()
	claudeDir := t.TempDir()
	sessDir := filepath.Join(claudeDir, "projects", "myproj")
	subDir := filepath.Join(sessDir, "sess1", "subagents")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess1.jsonl"), []byte(`{"type":"user","cwd":"/tmp"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(subDir, "agent-xyz.jsonl")
	writeFollowLines(t, transcript, n)
	meta := `{"finished":false}`
	if err := os.WriteFile(filepath.Join(subDir, "agent-xyz.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return ViewOptions{ClaudeDir: claudeDir, Session: "sess1", Agent: "xyz"}, transcript
}

func writeFollowLines(t *testing.T, path string, n int) {
	t.Helper()
	var lines []string
	for i := range n {
		lines = append(lines, buildAgentLine("line"+string(rune('a'+i))))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// When the transcript shrinks between two Follow polls (e.g. truncated/rewritten), `seen` must reset
// to 0 instead of indexing all[seen:] with seen > len(all), which panics (slice bounds out of range).
func TestFollowResetsSeenWhenEventCountShrinks(t *testing.T) {
	opts, transcript := followFixture(t, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 1300*time.Millisecond)
	defer cancel()

	shrunk := make(chan struct{})
	go func() {
		time.Sleep(650 * time.Millisecond) // after Follow's first poll (t=0) and first tick (t=500ms)
		writeFollowLines(t, transcript, 1)
		close(shrunk)
	}()

	panicked := make(chan any, 1)
	var buf bytes.Buffer
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			} else {
				panicked <- nil
			}
		}()
		_ = Follow(ctx, opts, &buf) // blocks until ctx's deadline (followDone is pinned false)
	}()
	<-shrunk

	if r := <-panicked; r != nil {
		t.Fatalf("Follow panicked on a shrinking event count: %v", r)
	}
	// The second poll (t=1000ms) sees all=1 < seen=3, resets to 0, and re-prints the one event the
	// third line now holds, rather than panicking or printing nothing.
	if !strings.Contains(buf.String(), `"linea"`) {
		t.Errorf("output = %q, want it to contain the re-seen event after the reset", buf.String())
	}
}
