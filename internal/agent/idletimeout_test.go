package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// Given beats keep arriving, Then the watch re-arms its one timer (no new timer per beat) and trips once when quiet.
func TestIdleTimeoutBeatReusesTimer(t *testing.T) {
	var kills atomic.Int32
	w := NewIdleTimeout("x", 60*time.Millisecond, func() { kills.Add(1) })
	defer w.Stop()
	first := w.timer
	for _, ev := range []string{"system", "assistant", "user"} {
		w.Beat(ev)
		time.Sleep(30 * time.Millisecond)
	}
	w.Pause() // a running tool holds the window off
	time.Sleep(120 * time.Millisecond)
	if kills.Load() != 0 {
		t.Fatal("tripped while paused or beats kept arriving")
	}
	w.Beat("user") // the tool answered: re-armed
	time.Sleep(150 * time.Millisecond)
	if w.timer != first || kills.Load() != 1 {
		t.Fatalf("same timer = %v, kills = %d; want the one timer re-armed and one trip", w.timer == first, kills.Load())
	}
}
