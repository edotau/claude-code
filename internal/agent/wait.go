package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// Wait states: done and error from the record, lost a dead running record, timeout unsettled.
const (
	WaitDone    = "done"
	WaitError   = "error"
	WaitLost    = "lost"
	WaitTimeout = "timeout"
)

// WaitResult is one dispatch's outcome from Wait.
type WaitResult struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Elapsed string `json:"elapsed"`
	Parent  string `json:"-"`              // text rows only; the JSON shape is unchanged
	Live    string `json:"live,omitempty"` // a timeout row's progress line

	Observed *Observed `json:"observed,omitempty"`
}

// waitRowWidth is the column budget of one `agents wait` row.
const waitRowWidth = 80

// Line is the `agents wait` row: id, state, elapsed, then `<- parent` for a nested dispatch.
func (w WaitResult) Line() string {
	row := fmt.Sprintf("%s %s %s", w.ID, w.State, w.Elapsed)
	if w.Live != "" {
		row += " · " + w.Live
	}
	if w.Parent == "" {
		return row
	}
	const arrow = " <- "
	return row + arrow + textutil.TruncateCells(w.Parent, max(waitRowWidth-textutil.DisplayWidth(row+arrow), 1), "…")
}

// Wait blocks until every dispatch in ids settles or timeout passes; a bad id fails first, ctx cancel errors.
func Wait(ctx context.Context, ids []string, timeout, poll time.Duration) ([]WaitResult, error) {
	recs := make([]Record, len(ids))
	for i, id := range ids {
		rec, err := LoadRecord(id)
		if err != nil {
			return nil, err
		}
		recs[i] = rec
	}
	out := make([]WaitResult, len(ids))
	deadline := time.After(timeout)
	for {
		if !waitPass(recs, out) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			for i := range recs {
				if out[i].State == "" {
					out[i] = WaitResult{ID: recs[i].ID, Parent: recs[i].Parent, State: WaitTimeout, Elapsed: waitElapsed(recs[i], WaitTimeout), Live: ProgressOf(recs[i], time.Now()).Line(progressWidth), Observed: ObservedOf(recs[i])}
				}
			}
			return out, nil
		case <-time.After(poll):
		}
	}
}

// waitPass refreshes each unsettled record into out and reports whether any is still pending.
func waitPass(recs []Record, out []WaitResult) bool {
	pending := false
	for i := range recs {
		if out[i].State != "" {
			continue
		}
		if rec, err := LoadRecord(recs[i].ID); err == nil {
			recs[i] = rec // an unreadable re-read keeps the last good record
		}
		if state := waitState(recs[i]); state != "" {
			out[i] = WaitResult{ID: recs[i].ID, Parent: recs[i].Parent, State: state, Elapsed: waitElapsed(recs[i], state), Observed: ObservedOf(recs[i])}
		} else {
			pending = true
		}
	}
	return pending
}

// waitState is rec's settled state, or "" while it still runs.
func waitState(rec Record) string {
	switch {
	case rec.Status == "ok":
		return WaitDone
	case rec.Status != "running":
		return WaitError
	case !rec.Alive():
		return WaitLost
	}
	return ""
}

// waitElapsed is the recorded run time of a finished dispatch, else the time since it started.
func waitElapsed(rec Record, state string) string {
	if state == WaitDone || state == WaitError {
		return (time.Duration(rec.DurationMs) * time.Millisecond).String()
	}
	return time.Since(rec.Time).Round(time.Second).String()
}

// WaitExit is the `agents wait` exit code: 1 any error or lost, else 4 any timeout, else 0.
func WaitExit(rows []WaitResult) int {
	code := 0
	for _, r := range rows {
		switch r.State {
		case WaitError, WaitLost:
			return 1
		case WaitTimeout:
			code = 4
		}
	}
	return code
}
