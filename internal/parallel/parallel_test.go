package parallel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/nonet"
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
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "exploded") || !strings.Contains(string(p.Stack), "parallel_test.go") {
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

func TestAllEdgesAndEveryPanic(t *testing.T) {
	for _, n := range []int{0, 1, 1000} {
		errs := All(n, func(int) error { panic("slot exploded") })
		if len(errs) != n {
			t.Fatalf("n=%d: %d results", n, len(errs))
		}
		for _, err := range errs {
			checkPanic(t, err)
		}
	}
}

func TestPanicNilValueIsAnError(t *testing.T) {
	err := All(1, func(int) error { panic(nil) })[0]
	var p *PanicError
	if !errors.As(err, &p) || p.Value == nil {
		t.Fatalf("panic(nil) = %v, want a PanicError with a value", err)
	}
}

// A slot that calls runtime.Goexit (t.Fatal does) never returns; it must read as a failure, not success.
func TestGoexitIsAFailureEverywhere(t *testing.T) {
	exit := func() { runtime.Goexit() }
	t.Run("All", func(t *testing.T) {
		errs := All(2, func(i int) error {
			if i == 0 {
				exit()
			}
			return nil
		})
		if errs[0] == nil || errs[1] != nil {
			t.Fatalf("All: %v", errs)
		}
	})
	t.Run("Go", func(t *testing.T) {
		done := make(chan error, 1)
		Go(exit, func(err error) { done <- err })
		select {
		case err := <-done:
			checkExit(t, err)
		case <-time.After(time.Second):
			t.Fatal("Go: Goexit not reported")
		}
	})
	t.Run("Map", func(t *testing.T) {
		got, err := Map(t.Context(), []int{1, 2, 3}, 1, func(_ context.Context, n int) (int, error) {
			if n == 2 {
				exit()
			}
			return n, nil
		})
		if err == nil {
			t.Fatalf("Map: %v with a nil error after a Goexit with items left", got)
		}
	})
	t.Run("Within", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, err := Within(t.Context(), time.Hour, func(context.Context) (int, error) { exit(); return 0, nil })
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Within: %v, want the Goexit, not the budget", err)
			}
		})
	})
}

func checkExit(t *testing.T, err error) {
	t.Helper()
	var p *PanicError
	if !errors.As(err, &p) || !strings.Contains(err.Error(), "Goexit") {
		t.Fatalf("want a Goexit PanicError, got %v", err)
	}
}

func TestPanicStackIsThePanickingGoroutines(t *testing.T) {
	err := All(1, func(int) error { return explode() })[0]
	var p *PanicError
	if !errors.As(err, &p) || !strings.Contains(string(p.Stack), "parallel.explode") {
		t.Fatalf("stack lacks the panicking frame:\n%v", err)
	}
}

//go:noinline
func explode() error { panic("explode exploded") }

func TestRethrow(t *testing.T) {
	Rethrow(nil, errors.New("plain"))
	defer func() {
		if got := recover(); got != "boom" {
			t.Fatalf("recovered %v, want the original panic value", got)
		}
	}()
	Rethrow(errors.New("plain"), fmt.Errorf("item 1: %w", &PanicError{Value: "boom"}))
	t.Fatal("contained panic was not re-raised")
}

func TestRethrowWritesOriginStackToStderr(t *testing.T) {
	var got any
	_, _, stderr := hosttest.CaptureOutput(t, func() int {
		defer func() { got = recover() }()
		Rethrow(&PanicError{Value: "boom", Stack: []byte("origin-frame-xyz\n")})
		return 0
	})
	if got != "boom" {
		t.Fatalf("re-panicked %v, want the original value", got)
	}
	if strings.Count(stderr, "origin-frame-xyz") != 1 || !strings.Contains(stderr, "boom") {
		t.Fatalf("stderr = %q, want the panic value and origin stack once", stderr)
	}
}
