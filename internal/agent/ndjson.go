package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// decodeLines skips blank lines and logs non-JSON output without losing subsequent events.
func decodeLines(br *bufio.Reader, lg eventLog, fn func(raw []byte) (stop bool, err error)) error {
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

var clock = time.Now

// transportTracker times a stream's events and records the retries it saw.
type transportTracker struct {
	start, last time.Time
	statuses    []int
	events      int
	first, gap  time.Duration
}

func (t *transportTracker) event(now time.Time) {
	if t.events == 0 {
		t.first = now.Sub(t.start)
	} else if g := now.Sub(t.last); g > t.gap {
		t.gap = g
	}
	t.events++
	t.last = now
}

func (t *transportTracker) retry(status *int) {
	s := 0
	if status != nil {
		s = *status
	}
	t.statuses = append(t.statuses, s)
}

func (t *transportTracker) lastStatus() int { return t.statuses[len(t.statuses)-1] }

// result is nil when the stream showed neither a stream_event nor a retry.
func (t *transportTracker) result() *Transport {
	if t.events == 0 && len(t.statuses) == 0 {
		return nil
	}
	return &Transport{Retries: len(t.statuses), RetryStatuses: t.statuses,
		FirstEventMs: t.first.Milliseconds(), MaxGapMs: t.gap.Milliseconds()}
}

// resultError is a failed turn whose result event was consumed, so the stream stays at a turn boundary.
type resultError struct{ error }

func (e resultError) Unwrap() error { return e.error }
