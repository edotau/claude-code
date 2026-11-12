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
	ended   bool // an end line was already in the initial lines: nothing more is followed
}

// newFollower reads the file once and returns its complete lines before any end line; a partial last line waits.
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
	body, ended := cutAtEndLine(data[:cut])
	w := &follower{f: f, idle: idle, n: bytes.Count(body, []byte{'\n'}), pending: data[cut:], ended: ended}
	return w, bytes.NewReader(body), nil
}

// cutAtEndLine returns the complete lines before the first {"end":true} line, and whether one was found.
func cutAtEndLine(data []byte) ([]byte, bool) {
	for off := 0; off < len(data); {
		i := bytes.IndexByte(data[off:], '\n') + 1
		if isEndLine(data[off : off+i]) {
			return data[:off], true
		}
		off += i
	}
	return data, false
}

// isEndLine reports whether line is exactly {"end":true}.
func isEndLine(line []byte) bool {
	var m map[string]any
	return json.Unmarshal(line, &m) == nil && len(m) == 1 && m["end"] == true
}

// next returns the complete lines appended since the last call, numbered from the file's first line.
func (w *follower) next() ([]parsedLine, [][]byte, error) {
	b, err := io.ReadAll(w.f)
	w.pending = append(w.pending, b...)
	var parsed []parsedLine
	var raw [][]byte
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			return parsed, raw, err
		}
		line := w.pending[:i+1]
		w.pending = w.pending[i+1:]
		w.n++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		parsed = append(parsed, decodeTaskLine(line, w.n))
		raw = append(raw, line)
	}
}

// run sends tasks built from appended lines to the returned channel until {"end":true}, idle or cancel.
func (w *follower) run(ctx context.Context, build func(parsedLine) batchTask) <-chan batchTask {
	out := make(chan batchTask)
	go func() {
		defer close(out)
		defer w.f.Close()
		if w.ended {
			return
		}
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

// poll sends the newly appended lines; more is false on {"end":true}, a read error (sent as a failed task) or cancel.
func (w *follower) poll(ctx context.Context, build func(parsedLine) batchTask, out chan<- batchTask) (sent, more bool) {
	lines, raw, err := w.next()
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
	if err != nil {
		select { // a Task with Err fails the batch: a read error must not look like the end of input
		case out <- batchTask{Err: fmt.Errorf("--follow: %w", err)}:
		case <-ctx.Done():
		}
		return sent, false
	}
	return sent, true
}

// retryFor reroutes t once to the first --retry-agent other than the agent it ran on; nil when there is none.
func retryFor(t batchTask, bo batchOpts, agents []string) func(context.Context) (*agent.Prepared, error) {
	for _, name := range agents {
		if name == t.Agent {
			continue
		}
		req := t.Req
		req.Agent, req.Fallback, req.Native = name, bo.Fallback, bo.Native
		req, err := confineLine(name, req) // the new agent's own lead fill and account check, as buildTask does
		cl := newClient()
		return func(ctx context.Context) (*agent.Prepared, error) {
			if err != nil {
				return nil, fmt.Errorf("--retry-agent %s: %w", name, err)
			}
			return cl.Prepare(ctx, req, nil)
		}
	}
	return nil
}
