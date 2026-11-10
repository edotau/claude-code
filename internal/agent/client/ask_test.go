package client

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"
)

func newAskTarget() *AskTarget {
	s := func() *string { return new(string) }
	b := func() *bool { return new(bool) }
	return &AskTarget{Name: s(), Provider: s(), Model: s(), Effort: s(), Workdir: s(), Session: s(),
		Tools: b(), ReadOnly: b(), NoTools: b(), Autonomous: b(), Fleet: b(), EnableMemory: b(), Direct: b(),
		NoPersist: b(), SafeMode: b(), DebugFile: s(), SystemPrompt: s(), AppendSystemPrompt: s(), OutputSchema: s(),
		Host: s(), RemoteBin: s(), Fallback: s(), Timeout: new(time.Duration), Attach: new([]string), KeepEnv: new([]string),
		PermMode: s(), PermAllow: new([]string), PermAsk: new([]string), PermDeny: new([]string)}
}

// A retried or backgrounded dispatch must keep --no-tools; without it the tools come back on.
func TestApplyRecordRoundTripsNoTools(t *testing.T) {
	s := newAskTarget()
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{NoTools: true}}, io.Discard)
	if !*s.NoTools || *s.Tools {
		t.Errorf("noTools=%v tools=%v; want noTools on, tools off", *s.NoTools, *s.Tools)
	}
	// Negative control: a record without NoTools clears an earlier value.
	*s.NoTools = true
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.NoTools || !*s.Tools {
		t.Errorf("noTools=%v tools=%v; want tools on, noTools off", *s.NoTools, *s.Tools)
	}
}

// A replayed read-only dispatch must stay read-only; the flag default would hand it write tools.
func TestApplyRecordRoundTripsReadOnly(t *testing.T) {
	s := newAskTarget()
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{ReadOnly: true, Tools: true}}, io.Discard)
	if !*s.ReadOnly || !*s.Tools {
		t.Errorf("readOnly=%v tools=%v; want both on", *s.ReadOnly, *s.Tools)
	}
	s.Apply(job.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.ReadOnly {
		t.Error("readOnly stayed on for a record without it")
	}
}

func TestResolveAskPromptBoundsStdin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = r
	writerDone := make(chan struct{})
	go writeOversizePrompt(w, writerDone)
	t.Cleanup(func() {
		os.Stdin = original
		_ = r.Close()
		_ = w.Close()
		<-writerDone
	})
	type result struct {
		prompt string
		err    error
	}
	results := make(chan result, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		prompt, err := ResolveAskPrompt("-")
		results <- result{prompt, err}
	}()
	t.Cleanup(func() { _ = r.Close(); <-readerDone })
	select {
	case got := <-results:
		want := agent.PromptTooLarge(agent.MaxPromptBytes+1, agent.MaxPromptBytes)
		if got.prompt != "" || !errors.Is(got.err, agent.ErrPromptTooLarge) || got.err.Error() != want.Error() {
			t.Fatalf("ResolveAskPrompt = (%q, %v), want empty prompt and %q", got.prompt, got.err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ResolveAskPrompt waited for EOF instead of stopping at MaxPromptBytes+1")
	}
}

func writeOversizePrompt(w *os.File, done chan<- struct{}) {
	defer close(done)
	chunk := []byte(strings.Repeat("x", 4<<10))
	for range 4096 {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
	// Keep the write end open until cleanup: even a finite oversized stream must not require EOF.
}
