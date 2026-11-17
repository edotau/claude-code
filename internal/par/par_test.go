package par

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestMapOrderAndLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, peak atomic.Int32
		got, err := Map(t.Context(), []int{0, 1, 2, 3, 4, 5}, 2, func(_ context.Context, n int) (int, error) {
			current := active.Add(1)
			for old := peak.Load(); current > old; old = peak.Load() {
				if peak.CompareAndSwap(old, current) {
					break
				}
			}
			defer active.Add(-1)
			time.Sleep(time.Duration(6-n) * time.Millisecond)
			return n * 2, nil
		})
		if err != nil || !reflect.DeepEqual(got, []int{0, 2, 4, 6, 8, 10}) {
			t.Fatalf("got %v, %v", got, err)
		}
		if peak.Load() != 2 || active.Load() != 0 {
			t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
		}
	})
}

func TestMapCancelsAndJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fail := errors.New("first")
		entered := make(chan struct{})
		var finished atomic.Bool
		_, err := Map(t.Context(), []int{0, 1, 2}, 2, func(ctx context.Context, n int) (int, error) {
			if n == 0 {
				<-entered
				return 0, fail
			}
			close(entered)
			<-ctx.Done()
			finished.Store(true)
			return 0, ctx.Err()
		})
		if !errors.Is(err, fail) || !finished.Load() {
			t.Fatalf("error=%v finished=%v", err, finished.Load())
		}
	})
}

func TestMapPanicAndEdges(t *testing.T) {
	_, err := Map(t.Context(), []int{1}, 1, func(context.Context, int) (int, error) { panic("item exploded") })
	checkPanic(t, err)
	for _, limit := range []int{-1, 0, 1} {
		got, err := Map(t.Context(), []int{1}, limit, func(_ context.Context, n int) (int, error) { return n, nil })
		if err != nil || !reflect.DeepEqual(got, []int{1}) {
			t.Fatalf("limit %d: %v %v", limit, got, err)
		}
	}
	got, err := Map(t.Context(), []int{}, 0, func(context.Context, int) (int, error) { t.Fatal("empty callback"); return 0, nil })
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Map(ctx, []int{1}, 2, func(context.Context, int) (int, error) { t.Fatal("cancelled callback"); return 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func checkPanic(t *testing.T, err error) {
	t.Helper()
	var p *PanicError
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "exploded") || !strings.Contains(string(p.Stack), "par_test.go") {
		t.Fatalf("panic missing value or stack: %v", err)
	}
}

func TestGoPanic(t *testing.T) {
	done := make(chan error, 1)
	Go(func() { panic("goroutine exploded") }, func(err error) { done <- err })
	checkPanic(t, <-done)
}

func TestWithin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got, err := Within(t.Context(), time.Second, func(context.Context) (int, error) { return 42, nil })
		if got != 42 || err != nil {
			t.Fatalf("%d %v", got, err)
		}
		_, err = Within(t.Context(), time.Second, func(context.Context) (int, error) { panic("budget exploded") })
		checkPanic(t, err)
		var finished atomic.Bool
		_, err = Within(t.Context(), time.Millisecond, func(ctx context.Context) (int, error) {
			<-ctx.Done()
			time.Sleep(time.Millisecond)
			defer finished.Store(true)
			panic("late exploded")
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		synctest.Wait()
		if !finished.Load() {
			t.Fatal("abandoned worker did not finish")
		}
	})
}

func TestAll(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	for _, tc := range []struct {
		name      string
		errs      []error
		panicSlot int
	}{
		{"success", make([]error, 4), -1},
		{"panic", make([]error, 4), 1},
		{"error", []error{nil, nil, first, nil}, -1},
		{"order", []error{first, second, nil, first}, -1},
		{"empty", nil, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(tc.errs)
			var finished atomic.Int32
			entered := make(chan struct{}, n)
			errs := All(n, func(i int) error {
				defer finished.Add(1)
				entered <- struct{}{}
				if i == 0 {
					for range n {
						<-entered
					}
				}
				if i == tc.panicSlot {
					panic("slot exploded")
				}
				return tc.errs[i]
			})
			if len(errs) != n || finished.Load() != int32(n) {
				t.Fatalf("errors=%v finished=%d", errs, finished.Load())
			}
			for i, err := range errs {
				if i == tc.panicSlot {
					checkPanic(t, err)
				} else if err != tc.errs[i] {
					t.Fatalf("slot %d: got %v want %v", i, err, tc.errs[i])
				}
			}
		})
	}
}
