package agent

import (
	"sync/atomic"
	"testing"
	"time"
)

// Given beats keep arriving, Then the watch re-arms its one timer (no new timer per beat) and trips once when quiet.
func TestIdleTimeoutBeatReusesTimer(t *testing.T) {
	var kills atomic.Int32
	const window = 200 * time.Millisecond
	w := NewIdleTimeout("x", window, func() { kills.Add(1) })
	defer w.Stop()
	first := w.timer
	for _, ev := range []string{"system", "assistant", "user"} {
		w.Beat(ev)
		time.Sleep(window / 10)
	}
	w.Pause() // a running tool holds the window off
	time.Sleep(window * 6 / 5)
	if kills.Load() != 0 {
		t.Fatal("tripped while paused or beats kept arriving")
	}
	w.Beat("user") // the tool answered: re-armed
	for deadline := time.Now().Add(5 * time.Second); kills.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if w.timer != first || kills.Load() != 1 {
		t.Fatalf("same timer = %v, kills = %d; want the one timer re-armed and one trip", w.timer == first, kills.Load())
	}
}
