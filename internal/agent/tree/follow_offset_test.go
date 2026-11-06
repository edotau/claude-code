package tree

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

const followOffsetQuietPeriod = 175 * time.Millisecond

func TestFollowHoldsPartialLineUntilComplete(t *testing.T) {
	path, emitted := startFollowOffset(t, 1)
	if pollFollowOffset(followOffsetQuietPeriod, func() bool { return len(emitted()) != 0 }) {
		t.Fatalf("replayed history: %+v", emitted())
	}
	f := openFollowOffsetAppend(t, path)
	text := "complete appended text"
	line := buildAgentLine(text)
	if _, err := f.WriteString(line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	if pollFollowOffset(followOffsetQuietPeriod, func() bool { return len(emitted()) != 0 }) {
		t.Fatalf("emitted an incomplete line: %+v", emitted())
	}
	if _, err := f.WriteString(line[len(line)/2:] + "\n"); err != nil {
		t.Fatal(err)
	}
	if !pollFollowOffset(2*time.Second, func() bool { return len(emitted()) != 0 }) {
		t.Fatal("complete appended line was not emitted")
	}
	if pollFollowOffset(followOffsetQuietPeriod, func() bool {
		got := emitted()
		return len(got) != 1 || got[0].Kind != "text" || got[0].Text != text
	}) {
		t.Fatalf("want exactly one complete text event, got %+v", emitted())
	}
}

func TestFollowEmitsOnlyAppendedLines(t *testing.T) {
	path, emitted := startFollowOffset(t, 2)
	if pollFollowOffset(followOffsetQuietPeriod, func() bool { return len(emitted()) != 0 }) {
		t.Fatalf("replayed history: %+v", emitted())
	}
	f := openFollowOffsetAppend(t, path)
	text := "new event only"
	if _, err := f.WriteString(buildAgentLine(text) + "\n"); err != nil {
		t.Fatal(err)
	}
	if !pollFollowOffset(2*time.Second, func() bool { return len(emitted()) != 0 }) {
		t.Fatal("appended line was not emitted")
	}
	if pollFollowOffset(followOffsetQuietPeriod, func() bool {
		got := emitted()
		return len(got) != 1 || got[0].Kind != "text" || got[0].Text != text
	}) {
		t.Fatalf("want only the appended event once, got %+v", emitted())
	}
}

func openFollowOffsetAppend(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func startFollowOffset(t *testing.T, n int) (string, func() []Event) {
	t.Helper()
	opts, path := followFixture(t, n)
	saved := followPollInterval
	followPollInterval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var mu sync.Mutex
	var emitted []Event
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, opts, func(e Event) {
			mu.Lock()
			emitted = append(emitted, e)
			mu.Unlock()
		})
	}()
	t.Cleanup(func() {
		cancel()
		err := <-done
		followPollInterval = saved
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Follow returned %v, want context cancellation", err)
		}
	})
	return path, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), emitted...)
	}
}

func pollFollowOffset(timeout time.Duration, condition func() bool) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-deadline.C:
			return condition()
		case <-ticker.C:
		}
	}
}
