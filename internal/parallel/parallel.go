// Package parallel runs bounded work with panic containment.
package parallel

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// PanicError carries the original panic value and goroutine stack.
type PanicError struct {
	Value any
	Stack []byte
}

// Error includes the panic value and its original stack.
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v\n%s", e.Value, e.Stack) }

// errGoexit is the Value of a PanicError for a function that left via runtime.Goexit instead of returning.
var errGoexit = errors.New("runtime.Goexit called")

// call runs fn and hands its outcome to deliver from a defer, so a panic or runtime.Goexit is still delivered once.
func call[T any](fn func() (T, error), deliver func(T, error)) {
	var v T
	var err error
	returned := false
	defer func() {
		if !returned {
			p := recover()
			if p == nil {
				p = errGoexit
			}
			err = &PanicError{Value: p, Stack: debug.Stack()}
		}
		deliver(v, err)
	}()
	v, err = fn()
	returned = true
}

// Go launches fn and reports a panic or Goexit to report, which must be non-nil and must not panic.
func Go(fn func(), report func(error)) {
	if report == nil {
		panic("parallel.Go: nil panic reporter")
	}
	go call(func() (struct{}, error) { fn(); return struct{}{}, nil }, func(_ struct{}, err error) {
		if err != nil {
			report(err)
		}
	})
}

// All joins every slot without letting a failure cancel its siblings.
func All(n int, fn func(int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			call(func() (struct{}, error) { return struct{}{}, fn(i) }, func(_ struct{}, err error) { errs[i] = err })
		})
	}
	wg.Wait()
	return errs
}

// Map preserves order, joins started work, and cancels on first error; limit <= 0 means one worker.
func Map[A, B any](ctx context.Context, items []A, limit int, fn func(context.Context, A) (B, error)) ([]B, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	out := make([]B, len(items))
	var wg sync.WaitGroup
	var next int
	var mu sync.Mutex
	for range min(max(limit, 1), len(items)) {
		wg.Go(func() {
			for {
				mu.Lock()
				if next == len(items) || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				call(func() (B, error) { return fn(ctx, items[i]) }, func(v B, err error) {
					out[i] = v
					if err != nil {
						cancel(fmt.Errorf("item %d: %w", i, err))
					}
				})
			}
		})
	}
	wg.Wait()
	return out, context.Cause(ctx)
}

// Within cancels on expiry; abandoned work can finish safely, including a late panic.
func Within[T any](ctx context.Context, budget time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go call(func() (T, error) { return fn(ctx) }, func(v T, err error) { done <- result{v, err} })
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	}
}
