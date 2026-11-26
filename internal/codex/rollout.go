package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

const rolloutPoll = 2 * time.Second

type lockedSink struct {
	mu sync.Mutex
	wire.Sink
}

func (l *lockedSink) Emit(ev wire.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Sink.Emit(ev)
}

// tailRollout emits live usage from the thread's rollout file until the returned stop is called, which joins it.
func tailRollout(threadID string, sink wire.Sink) (stop func()) {
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		var path string
		var off int64
		tick := time.NewTicker(rolloutPoll)
		defer tick.Stop()
		for {
			if path == "" {
				path = findRollout(threadID)
			}
			if path != "" {
				var u *wire.Usage
				if off, u = readTokenCounts(path, off); u != nil {
					_ = sink.Emit(wire.Event{Type: wire.EventUsage, Usage: u})
				}
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() { close(done); <-exited }
}

// findRollout matches any date dir: codex names it by its own clock. Every error means no live tokens.
func findRollout(threadID string) string {
	matches, err := filepath.Glob(filepath.Join(codexHome(), "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// readTokenCounts returns the offset past the last whole line and the newest cumulative total in the new lines.
func readTokenCounts(path string, off int64) (int64, *wire.Usage) {
	f, err := os.Open(path)
	if err != nil {
		return off, nil
	}
	defer f.Close()
	if _, err = f.Seek(off, io.SeekStart); err != nil {
		return off, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return off, nil
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	var last *wire.Usage
	for line := range bytes.SplitSeq(data[:end], []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`"token_count"`)) { // skip decoding the bulky item records
			continue
		}
		var rec struct {
			Payload struct {
				Type string `json:"type"`
				Info struct {
					Total struct {
						Input  int `json:"input_tokens"`
						Cached int `json:"cached_input_tokens"`
						Output int `json:"output_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Payload.Type != "token_count" {
			continue
		}
		t := rec.Payload.Info.Total // same mapping as turn.completed in stream.go
		last = &wire.Usage{InputTokens: max(0, t.Input-t.Cached), CacheReadTokens: t.Cached, OutputTokens: t.Output}
	}
	return off + int64(end), last
}
