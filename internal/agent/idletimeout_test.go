package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// Given beats keep arriving, Then the watch re-arms its one timer (no new timer per beat) and trips once when quiet.
func TestIdleTimeoutBeatReusesTimer(t *testing.T) {
	var kills atomic.Int32
	const window = 40 * time.Millisecond
	w := NewIdleTimeout("x", time.Hour, func() { kills.Add(1) }) // no wall-clock trip while beating or paused
	defer w.Stop()
	first := w.timer
	for _, ev := range []string{"system", "assistant", "user"} {
		w.Beat(ev)
	}
	w.Pause() // a running tool holds the window off
	w.mu.Lock()
	w.due = time.Now().Add(-time.Second)
	w.mu.Unlock()
	w.trip() // a fire that raced the Pause, already past due
	if kills.Load() != 0 {
		t.Fatal("tripped while paused or beats kept arriving")
	}
	w.Rearm(window, "user") // the tool answered: re-armed on the short window
	for deadline := time.Now().Add(5 * time.Second); kills.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(window / 40)
	}
	if w.timer != first || kills.Load() != 1 {
		t.Fatalf("same timer = %v, kills = %d; want the one timer re-armed and one trip", w.timer == first, kills.Load())
	}
}
