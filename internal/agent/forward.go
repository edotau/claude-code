package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
)

// forwardPoll is how often the forwarder looks for the child and re-reads its event file.
var forwardPoll = 50 * time.Millisecond

// forwardTextCap bounds one forwarded line's text in runes, as the log's own event lines are.
const forwardTextCap = 300

// StartForward runs ForwardEvents in one goroutine; stop cancels it and waits for its final drain.
func StartForward(ctx context.Context, parent, skip string, log wire.StreamLog) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ForwardEvents(ctx, parent, skip, log)
	}()
	return func() { cancel(); <-done }
}

// ForwardEvents tails the events of parent's newest child but skip into log; it reads a file, never the child's pipe.
func ForwardEvents(ctx context.Context, parent, skip string, log wire.StreamLog) {
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close() // read-only: a close error loses nothing
		}
	}()
	var reader *bufio.Reader
	var pending []byte
	w := forwardWriter{log: log}
	for {
		cancelled := ctx.Err() != nil
		if f == nil {
			var err error
			if f, w.child, err = forwardSource(parent, skip); err != nil {
				w.emit(wire.Event{Type: wire.EventError, Text: err.Error()})
				return
			}
			if f != nil {
				reader = bufio.NewReader(f)
			}
		}
		if reader != nil {
			if _, err := copyEventLines(reader, &pending, 0, w); err != nil {
				w.emit(wire.Event{Type: wire.EventError, Text: err.Error()})
				return
			}
		}
		if cancelled {
			return
		}
		_ = sleepCtx(ctx, forwardPoll) // a cancel ends the wait early; the next pass is the final drain
	}
}

// forwardSource opens the child's event file; a child not yet recorded or not yet writing is nil and no error.
func forwardSource(parent, skip string) (*os.File, string, error) {
	child, ok := LatestChild(parent)
	if !ok || child.ID == skip {
		return nil, "", nil
	}
	f, err := os.Open(child.EventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, child.ID, nil
	}
	return f, child.ID, err
}

// forwardWriter takes one event line at a time from copyEventLines.
type forwardWriter struct {
	child string
	log   wire.StreamLog
}

func (w forwardWriter) Write(p []byte) (int, error) {
	var ev wire.Event
	if json.Unmarshal(p, &ev) == nil { // an undecodable line is skipped, never a reason to stop forwarding
		w.emit(ev)
	}
	return len(p), nil
}

// emit tags ev for the log; text (already streamed) and the result (the carrier's own) are not forwarded.
func (w forwardWriter) emit(ev wire.Event) {
	switch ev.Type {
	case wire.EventText, wire.EventProgress, wire.EventResult:
		return
	}
	tool := ""
	if ev.Tool != nil {
		tool = ev.Tool.Name
	}
	text := strings.Join([]string{"via " + w.child + ":", tool, ev.Status, ev.Text}, " ")
	text = strings.Join(strings.Fields(redact.Text(textutil.RedactSecrets(text))), " ")
	w.log.Emit(ev.Type, textutil.Truncate(text, forwardTextCap))
}
