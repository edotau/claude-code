package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/tokenledger"
)

// ErrSilent is a stall after the child's first model output: a slow turn, so the account is not benched for it.
var ErrSilent = errors.New("dispatch went silent mid-stream past the idle limit")

// IdleTimeout kills a child whose stream stays quiet for d: armed at creation, re-armed by Beat, held off by Pause.
type IdleTimeout struct {
	name    string
	d       time.Duration
	mu      sync.Mutex
	kill    func()
	timer   *time.Timer
	armed   bool      // false while paused
	due     time.Time // the current window's end, so a fire that raced a Reset cannot trip the newer window
	last    string
	spoke   bool // the child produced model output: a later trip is ErrSilent, not ErrStalled
	tooled  bool // a tool call ran (ToolCall): a trip is a budget end even before any model text
	tripped bool
	done    bool
	deliver bool // the child already delivered its final result: a later trip only bounds a lingering child
}

// NewIdleTimeout arms a d-long idle window now; d <= 0 returns nil, whose methods are all no-ops. kill may be nil.
func NewIdleTimeout(name string, d time.Duration, kill func()) *IdleTimeout {
	if d <= 0 {
		return nil
	}
	w := &IdleTimeout{name: name, d: d, kill: kill, last: "none"}
	w.mu.Lock()
	w.armed, w.due = true, time.Now().Add(d) // due before the timer starts: a later due made the fire look early and dropped it
	w.timer = time.AfterFunc(d, w.trip)
	w.mu.Unlock()
	return w
}

func (w *IdleTimeout) trip() {
	w.mu.Lock()
	if !w.armed || w.done || w.tripped {
		w.mu.Unlock()
		return
	}
	if left := time.Until(w.due); left > 0 { // fired early: re-arm for the rest, never drop the window
		w.timer.Reset(left)
		w.mu.Unlock()
		return
	}
	w.tripped = true
	kill := w.kill
	w.mu.Unlock()
	if kill != nil {
		kill()
	}
}

// Kill ends the window now as a trip, running the kill once; a later Err reads as a stall unless the caller classifies it.
func (w *IdleTimeout) Kill() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.done || w.tripped {
		w.mu.Unlock()
		return
	}
	w.tripped = true
	w.timer.Stop()
	kill := w.kill
	w.mu.Unlock()
	if kill != nil {
		kill()
	}
}

// bind sets the kill to cancel once the child has started; a trip that came first kills it now.
func (w *IdleTimeout) bind(cancel func() error) {
	if w == nil || cancel == nil {
		return
	}
	kill := func() { _ = cancel() }
	w.mu.Lock()
	w.kill = kill
	tripped := w.tripped
	w.mu.Unlock()
	if tripped {
		kill()
	}
}

// Beat records event what and restarts the idle window, unless the watch is disarmed, stopped or tripped.
func (w *IdleTimeout) Beat(what string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = what
	if w.done || w.tripped {
		return
	}
	w.armed, w.due = true, time.Now().Add(w.d)
	w.timer.Reset(w.d)
}

// pulse restarts the window on token flow; unlike Beat it leaves a paused (tool running) window paused.
func (w *IdleTimeout) pulse() {
	if w == nil {
		return
	}
	w.mu.Lock()
	armed := w.armed
	w.mu.Unlock()
	if armed {
		w.Beat("tokens")
	}
}

// tokenFeed fans a run's token movement out to its idle watches: grown usage is a sign of life on a quiet stream.
type tokenFeed struct {
	mu      sync.Mutex
	total   int
	watches []*IdleTimeout
	ledger  *tokenledger.Follower // the proxy's entries for this dispatch: token flow a silent stream never reports
	next    time.Time
}

// ledgerPollDiv sets the proxy-ledger poll to this fraction of IdleLimit, so a quiet run is read many times per window.
const ledgerPollDiv = 20

type tokenFeedKey struct{}

// withTokenFeed gives ctx's run a feed; the run's tally notes each usage event on it.
func withTokenFeed(ctx context.Context) (context.Context, *tokenFeed) {
	f := &tokenFeed{}
	return context.WithValue(ctx, tokenFeedKey{}, f), f
}

func (f *tokenFeed) note(u *wire.Usage) {
	if f == nil || u == nil {
		return
	}
	f.mu.Lock()
	grew := u.Total() > f.total
	f.total = max(f.total, u.Total())
	ws := slices.Clone(f.watches)
	f.mu.Unlock()
	if grew {
		for _, w := range ws {
			w.pulse()
		}
	}
}

// poll pulses the watches when the ledger holds new tokens for dispatch id; due at most once per ledgerPollDiv.
func (f *tokenFeed) poll(id string, now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if len(f.watches) == 0 || now.Before(f.next) {
		f.mu.Unlock()
		return
	}
	f.next = now.Add(IdleLimit / ledgerPollDiv)
	if f.ledger == nil {
		f.ledger = tokenledger.NewFollower()
	}
	ledger, ws := f.ledger, slices.Clone(f.watches)
	f.mu.Unlock()
	if n, _ := ledger.Next(id); n > 0 {
		for _, w := range ws {
			w.pulse()
		}
	}
}

// Follow counts token flow on ctx's run as a beat; a driver calls it right after NewIdleTimeout.
func (w *IdleTimeout) Follow(ctx context.Context) {
	f, _ := ctx.Value(tokenFeedKey{}).(*tokenFeed)
	if w == nil || f == nil {
		return
	}
	f.mu.Lock()
	f.watches = append(f.watches, w)
	f.mu.Unlock()
}

// Rearm switches the window length to d and restarts it (the first-output window gives way to the idle one).
func (w *IdleTimeout) Rearm(d time.Duration, what string) {
	if w == nil || d <= 0 {
		return
	}
	w.mu.Lock()
	w.d = d
	w.mu.Unlock()
	w.Beat(what)
}

// Pause holds the window off (a tool is running) until the next Beat.
func (w *IdleTimeout) Pause() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
	w.armed = false
}

// ToolCall marks that the child started a tool call, so a later trip is a budget end even before any model text.
func (w *IdleTimeout) ToolCall() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.tooled = true
	w.mu.Unlock()
}

// Spoke marks the child's first model output, so a later trip reads as a mid-stream ErrSilent.
func (w *IdleTimeout) Spoke() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.spoke = true
	w.mu.Unlock()
}

// Delivered marks the final result as received: a later trip kills a lingering child but no longer fails the run.
func (w *IdleTimeout) Delivered() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.deliver = true
	w.mu.Unlock()
}

// HasDelivered reports whether Delivered was called.
func (w *IdleTimeout) HasDelivered() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deliver
}

// Stop ends the watch for good (first output, or the stream read); a trip that already happened still reports via Err.
func (w *IdleTimeout) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.timer.Stop()
}

// Err is the classified failure when the window expired, else nil: ErrSilent after Spoke, ErrStalled before.
func (w *IdleTimeout) Err() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.tripped || w.deliver {
		return nil
	}
	sentinel := ErrStalled
	if w.spoke {
		sentinel = ErrSilent
	}
	e := &retry.Error{Kind: retry.KindFatal, Agent: w.name, Err: fmt.Errorf("%w: %s was silent for %s (last event: %s)", sentinel, w.name, w.d, w.last)}
	if w.spoke || w.tooled { // a stall before any output or tool call stays transport (A1)
		e.Budget = retry.BudgetIdle
	}
	return e
}
