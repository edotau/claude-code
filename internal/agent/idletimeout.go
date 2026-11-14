package agent

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
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
	w.timer = time.AfterFunc(d, w.trip)
	w.armed, w.due = true, time.Now().Add(d)
	w.mu.Unlock()
	return w
}

func (w *IdleTimeout) trip() {
	w.mu.Lock()
	if !w.armed || time.Now().Before(w.due) || w.done || w.tripped {
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
// A delivered result makes it nil.
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
	return retry.Fatal(w.name, fmt.Errorf("%w: %s was silent for %s (last event: %s)", sentinel, w.name, w.d, w.last))
}
