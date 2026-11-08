package client

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

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
