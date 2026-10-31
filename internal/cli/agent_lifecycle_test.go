package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// seedClaudeJob saves a finished claude record with a log and, when events is non-nil, an events file.
func seedClaudeJob(t *testing.T, at time.Time, events []agent.Event) agent.Record {
	t.Helper()
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "say hi"}, at)
	rec.Repo = t.TempDir()
	rec.Status, rec.SessionID, rec.DurationMs = "ok", "sess-1", 42
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec.LogPath(), []byte("hi there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if events != nil {
		var b strings.Builder
		for _, ev := range events {
			line, _ := json.Marshal(ev)
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

// TestAgentsJobLifecycleClaude: status/result/tail resolve a claude dispatch by id and read its events.
func TestAgentsJobLifecycleClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env := agent.NewEnvelope(agent.Result{Agent: "claude", Answer: "from-event", SessionID: "sess-1"}, nil, "")
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	env.DispatchID = rec.ID
	evs := []agent.Event{
		{Schema: 1, Seq: 0, Type: agent.EventStart},
		{Schema: 1, Seq: 1, Type: agent.EventText, Text: "hi"},
		{Schema: 1, Seq: 2, Type: agent.EventResult, Envelope: &env},
	}
	var b strings.Builder
	for _, ev := range evs {
		line, _ := json.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(rec.EventsPath(), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("status", []string{rec.ID}) })
	if code != 0 || !strings.Contains(out, rec.ID) || !strings.Contains(out, "ok") {
		t.Errorf("status = %d %q, want 0 with id and ok", code, out)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "from-event" || got.DispatchID != rec.ID {
		t.Errorf("envelope = %+v, want the result event's", got)
	}

	out = captureStdout(t, func() { code = cmdAgentsJob("tail", []string{rec.ID, "--format", "json", "-f"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("tail = %d, %d lines %q, want 3 events then exit", code, len(lines), out)
	}
	for i, want := range []string{agent.EventStart, agent.EventText, agent.EventResult} {
		var ev agent.Event
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil || ev.Type != want {
			t.Errorf("line %d = %q, want type %s", i, lines[i], want)
		}
	}
}

// TestAgentsResultJSONWithoutEvents: an old record with no events file rebuilds its envelope from record + log.
func TestAgentsResultJSONWithoutEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)

	var code int
	out := captureStdout(t, func() { code = cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, out, err)
	}
	if got.Answer != "hi there\n" || got.SessionID != "sess-1" || got.DispatchID != rec.ID || got.Agent != "claude" {
		t.Errorf("rebuilt envelope = %+v, want log answer, session and id from the record", got)
	}
}

// TestAgentsResultWarnsOnUnreadableEvents: an oversized event line warns on stderr and stdout stays a valid envelope.
func TestAgentsResultWarnsOnUnreadableEvents(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if err := os.WriteFile(rec.EventsPath(), []byte(strings.Repeat("x", 17<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := captureOutput(t, func() int { return cmdAgentsJob("result", []string{rec.ID, "--format", "json"}) })
	var got agent.Envelope
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || code != 0 {
		t.Fatalf("result json = %d %q: %v", code, stdout, err)
	}
	if got.Answer != "hi there\n" {
		t.Errorf("answer = %q, want the log rebuilt into the envelope", got.Answer)
	}
	if !strings.Contains(stderr, rec.ID) || !strings.Contains(stderr, "token too long") {
		t.Errorf("stderr = %q, want a warning naming the dispatch and the scan error", stderr)
	}
}

// TestAgentsCancelNotRunning: cancel on a finished record exits non-zero.
func TestAgentsCancelNotRunning(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := seedClaudeJob(t, time.Now().Add(-time.Hour), nil)
	if code := cmdAgentsJob("cancel", []string{rec.ID}); code == 0 {
		t.Error("cancel of a finished job exited 0, want an error")
	}
}
