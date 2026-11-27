package statusline

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type parentTTYLookup func(context.Context, int) (int, string, error)

func testPSParentTTYNative(t *testing.T, lookup parentTTYLookup) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	ppid, _, err := lookup(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getppid(); ppid != want {
		t.Fatalf("parent = %d, want %d", ppid, want)
	}
}

func testPSParentTTYInvalidPID(t *testing.T, lookup parentTTYLookup) {
	t.Helper()
	for _, pid := range []int{-1, 0, 1<<31 - 1} {
		if _, _, err := lookup(context.Background(), pid); err == nil {
			t.Errorf("pid %d: expected an error", pid)
		}
	}
}

func testPSParentTTYContext(t *testing.T, lookup parentTTYLookup) {
	t.Helper()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{canceled, expired} {
		if _, _, err := lookup(ctx, os.Getpid()); !errors.Is(err, ctx.Err()) {
			t.Errorf("error = %v, want %v", err, ctx.Err())
		}
	}
}
