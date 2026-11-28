package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/tokenledger"
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

// Token movement keeps a quiet run alive; with none, the idle window trips.
func TestIdleTimeoutTokenFlowKeepsRunAlive(t *testing.T) {
	ctx, feed := withTokenFeed(context.Background())
	tripped := make(chan struct{}, 1)
	w := NewIdleTimeout("x", 60*time.Millisecond, func() { tripped <- struct{}{} })
	w.Follow(ctx)
	for i := 1; i <= 6; i++ {
		time.Sleep(25 * time.Millisecond)
		feed.note(&wire.Usage{OutputTokens: i * 10})
	}
	select {
	case <-tripped:
		t.Fatal("stopped while tokens were flowing")
	default:
	}
	feed.note(&wire.Usage{OutputTokens: 60}) // unchanged total is no movement
	select {
	case <-tripped:
	case <-time.After(time.Second):
		t.Fatal("no idle stop without token movement")
	}
}

// The ledger poll pulses a watch only for its own dispatch's entries.
func TestTokenFeedPollFollowsOwnDispatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, feed := withTokenFeed(context.Background())
	tripped := make(chan struct{}, 1)
	w := NewIdleTimeout("x", 80*time.Millisecond, func() { tripped <- struct{}{} })
	w.Follow(ctx)
	feed.poll("d-1", time.Now()) // creates the follower at the ledger's end
	tokenledger.Append(tokenledger.Entry{Dispatch: "d-2", Input: 5})
	for range 4 {
		time.Sleep(30 * time.Millisecond)
		tokenledger.Append(tokenledger.Entry{Dispatch: "d-1", Output: 7})
		feed.next = time.Time{}
		feed.poll("d-1", time.Now())
	}
	select {
	case <-tripped:
		t.Fatal("stopped while its dispatch's ledger entries flowed")
	default:
	}
	for range 4 {
		time.Sleep(30 * time.Millisecond)
		feed.next = time.Time{}
		feed.poll("d-1", time.Now())
	}
	select {
	case <-tripped:
	default:
		t.Fatal("no idle stop once only another dispatch's entries arrived")
	}
}
