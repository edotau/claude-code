package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DecodeEvents decodes br's JSON lines into E for handle, timing each when tick; blank and non-JSON lines are
// skipped (non-JSON logged raw), and a valid line that does not fit E runs onBad, then aborts.
func DecodeEvents[E any](br *bufio.Reader, lg StreamLog, label string, tick bool, onBad func(error),
	handle func(E, *TransportTracker) (stop bool, err error)) (*Transport, error) {
	ts := TransportTracker{Start: Clock()}
	for {
		raw, readErr := br.ReadBytes('\n')
		if stop, err := decodeLine(bytes.TrimSpace(raw), &ts, lg, label, tick, onBad, handle); err != nil || stop {
			return ts.Result(), err
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return ts.Result(), nil
			}
			return ts.Result(), readErr
		}
	}
}

// decodeLine handles one trimmed line for DecodeEvents; stop ends the stream cleanly, err aborts it.
func decodeLine[E any](raw []byte, ts *TransportTracker, lg StreamLog, label string, tick bool, onBad func(error),
	handle func(E, *TransportTracker) (bool, error)) (stop bool, err error) {
	if len(raw) == 0 {
		return false, nil
	}
	var ev E
	if err := json.Unmarshal(raw, &ev); err != nil {
		if _, ok := errors.AsType[*json.SyntaxError](err); ok {
			lg.Emit("raw", string(raw))
			return false, nil
		}
		err = fmt.Errorf("%s: %w", label, err)
		if onBad != nil {
			onBad(err)
		}
		return false, err
	}
	if tick {
		ts.Event(Clock())
	}
	return handle(ev, ts)
}

var Clock = time.Now

// TransportTracker times a stream's events and records the retries it saw.
type TransportTracker struct {
	Start, last time.Time
	statuses    []int
	events      int
	first, gap  time.Duration
}

func (t *TransportTracker) Event(now time.Time) {
	if t.events == 0 {
		t.first = now.Sub(t.Start)
	} else if g := now.Sub(t.last); g > t.gap {
		t.gap = g
	}
	t.events++
	t.last = now
}

func (t *TransportTracker) Retry(status *int) {
	s := 0
	if status != nil {
		s = *status
	}
	t.statuses = append(t.statuses, s)
}

func (t *TransportTracker) LastStatus() int { return t.statuses[len(t.statuses)-1] }

// Result is nil when the stream showed neither a stream_event nor a retry.
func (t *TransportTracker) Result() *Transport {
	if t.events == 0 && len(t.statuses) == 0 {
		return nil
	}
	return &Transport{Retries: len(t.statuses), RetryStatuses: t.statuses,
		FirstEventMs: t.first.Milliseconds(), MaxGapMs: t.gap.Milliseconds()}
}

// ResultError is a failed turn whose result event was consumed, so the stream stays at a turn boundary.
type ResultError struct{ error }

func (e ResultError) Unwrap() error { return e.error }

// NewResultError marks err as a failed turn whose result event was consumed.
func NewResultError(err error) error { return ResultError{err} }
