// Package par runs bounded work with panic containment.
package par

import (
	"context"
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

func call[T any](fn func() (T, error)) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &PanicError{Value: p, Stack: debug.Stack()}
		}
	}()
	return fn()
}

// Go launches fn and reports panics to report, which must be non-nil and must not panic.
func Go(fn func(), report func(error)) {
	if report == nil {
		panic("par.Go: nil panic reporter")
	}
	go func() {
		_, err := call(func() (struct{}, error) { fn(); return struct{}{}, nil })
		if err != nil {
			report(err)
		}
	}()
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
				v, err := call(func() (B, error) { return fn(ctx, items[i]) })
				out[i] = v
				if err != nil {
					cancel(fmt.Errorf("item %d: %w", i, err))
					return
				}
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
	Go(func() {
		v, err := call(func() (T, error) { return fn(ctx) })
		done <- result{v, err}
	}, func(err error) { done <- result{err: err} })
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	}
}
