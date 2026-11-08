package claude

import (
	"fmt"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agent"
)

// claudeIdleLimit is how long a claude child may emit nothing while a model response is expected; tests shorten it.
var claudeIdleLimit = 90 * time.Second

// stallWatch kills a silent claude child: armed from start, disarmed while a tool runs, reset by every event.
type stallWatch struct {
	d       time.Duration
	kill    func()
	mu      sync.Mutex
	timer   *time.Timer
	gen     int // bumps on every reset, so a timer that fired late cannot trip a newer window
	pending int // tool_use blocks emitted and not yet answered by a tool_result
	last    string
	tripped bool
	done    bool
}

// newStallWatch arms the idle window now; kill runs once if it expires.
func newStallWatch(d time.Duration, kill func()) *stallWatch {
	w := &stallWatch{d: d, kill: kill, last: "none"}
	w.mu.Lock()
	w.reset()
	w.mu.Unlock()
	return w
}

// reset restarts the idle window; the caller holds mu.
func (w *stallWatch) reset() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
	gen := w.gen
	w.timer = time.AfterFunc(w.d, func() { w.trip(gen) })
}

func (w *stallWatch) trip(gen int) {
	w.mu.Lock()
	if gen != w.gen || w.done || w.tripped {
		w.mu.Unlock()
		return
	}
	w.tripped = true
	w.mu.Unlock()
	w.kill()
}

// observe records an event: it resets the window unless a tool is running, which disarms it.
func (w *stallWatch) observe(ev claudeEvent) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = ev.Type
	if w.done || w.tripped {
		return
	}
	if ev.ParentToolUseID == "" {
		switch ev.Type {
		case "assistant":
			w.pending += countBlocks(ev, "tool_use")
		case "user":
			w.pending = max(w.pending-max(countBlocks(ev, "tool_result"), 1), 0)
		}
	}
	if w.pending > 0 || ev.Type == "result" {
		w.timer.Stop()
		w.gen++
		return
	}
	w.reset()
}

// countBlocks counts ev's message content blocks of the given type.
func countBlocks(ev claudeEvent, typ string) (n int) {
	for _, b := range ev.object().Content {
		if b.Type == typ {
			n++
		}
	}
	return n
}

// stop ends the watch for good.
func (w *stallWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.timer.Stop()
}

// stalled is the classified failure when the idle window expired, else nil.
func (w *stallWatch) stalled() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.tripped {
		return nil
	}
	err := fmt.Errorf("%w: claude was silent for %s (last event: %s)", agent.ErrStalled, w.d, w.last)
	return &retry.Error{Kind: retry.KindFatal, Agent: "claude", Err: err}
}
