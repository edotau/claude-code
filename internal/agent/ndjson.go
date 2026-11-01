package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// DecodeLines skips blank lines and logs non-JSON output without losing subsequent events.
func DecodeLines(br *bufio.Reader, lg StreamLog, fn func(raw []byte) (stop bool, err error)) error {
	for {
		raw, readErr := br.ReadBytes('\n')
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 {
			if !json.Valid(raw) {
				lg.emit("raw", string(raw))
			} else {
				stop, err := fn(raw)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
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

func (t *TransportTracker) retry(status *int) {
	s := 0
	if status != nil {
		s = *status
	}
	t.statuses = append(t.statuses, s)
}

func (t *TransportTracker) lastStatus() int { return t.statuses[len(t.statuses)-1] }

// Result is nil when the stream showed neither a stream_event nor a retry.
func (t *TransportTracker) Result() *Transport {
	if t.events == 0 && len(t.statuses) == 0 {
		return nil
	}
	return &Transport{Retries: len(t.statuses), RetryStatuses: t.statuses,
		FirstEventMs: t.first.Milliseconds(), MaxGapMs: t.gap.Milliseconds()}
}

// resultError is a failed turn whose result event was consumed, so the stream stays at a turn boundary.
type resultError struct{ error }

func (e resultError) Unwrap() error { return e.error }
