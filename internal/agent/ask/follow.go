package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// followPoll is how often the follower re-reads the batch file.
const followPoll = 200 * time.Millisecond

// follower tails the --batch file after its initial lines; it owns the file offset and the partial last line.
type follower struct {
	f       *os.File
	idle    time.Duration
	n       int // physical lines consumed so far
	pending []byte
}

// newFollower reads the file once and returns its complete lines; a trailing partial line waits for its newline.
func newFollower(src string, idle time.Duration) (*follower, io.Reader, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	cut := bytes.LastIndexByte(data, '\n') + 1
	return &follower{f: f, idle: idle, n: bytes.Count(data[:cut], []byte{'\n'}), pending: data[cut:]}, bytes.NewReader(data[:cut]), nil
}

// isEndLine reports whether line is exactly {"end":true}.
func isEndLine(line []byte) bool {
	var m map[string]any
	return json.Unmarshal(line, &m) == nil && len(m) == 1 && m["end"] == true
}

// next returns the complete lines appended since the last call, numbered from the file's first line.
func (w *follower) next() ([]ParsedLine, [][]byte, error) {
	b, err := io.ReadAll(w.f)
	w.pending = append(w.pending, b...)
	var parsed []ParsedLine
	var raw [][]byte
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			return parsed, raw, err
		}
		line := w.pending[:i+1]
		w.pending = w.pending[i+1:]
		w.n++
		pl, _ := ReadTaskLines(bytes.NewReader(line))
		for _, p := range pl {
			p.N = w.n
			parsed = append(parsed, p)
			raw = append(raw, line)
		}
	}
}

// run sends tasks built from appended lines to the returned channel until {"end":true}, idle or cancel.
func (w *follower) run(ctx context.Context, build func(ParsedLine) Task) <-chan Task {
	out := make(chan Task)
	go func() {
		defer close(out)
		defer w.f.Close()
		last := time.Now()
		tick := time.NewTicker(followPoll)
		defer tick.Stop()
		for {
			sent, more := w.poll(ctx, build, out)
			if sent {
				last = time.Now()
			}
			if !more || time.Since(last) >= w.idle {
				return
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// poll sends the newly appended lines; more is false on {"end":true}, a read error or cancel.
func (w *follower) poll(ctx context.Context, build func(ParsedLine) Task, out chan<- Task) (sent, more bool) {
	lines, raw, err := w.next()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: ask: --follow: %v\n", err)
		return false, false
	}
	for i, l := range lines {
		if isEndLine(raw[i]) {
			return sent, false
		}
		select {
		case out <- build(l):
			sent = true
		case <-ctx.Done():
			return sent, false
		}
	}
	return sent, true
}

// retryFor reroutes t once to the first --retry-agent other than the agent it ran on; nil when there is none.
func retryFor(t Task, bo Opts, agents []string) func(context.Context) (*agent.Prepared, error) {
	for _, name := range agents {
		if name == t.Agent {
			continue
		}
		req := t.Req
		req.Agent, req.Fallback, req.Native = name, bo.Fallback, bo.Native
		client := NewClient()
		return func(ctx context.Context) (*agent.Prepared, error) { return client.Prepare(ctx, req, nil) }
	}
	return nil
}
