package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

const (
	keepaliveEvery   = 60 * time.Second
	keepaliveJitter  = 20 * time.Second
	keepaliveWindow  = 24 * time.Hour // a profile is kept alive this long after its last request
	keepaliveTimeout = 30 * time.Second
)

// keepalive holds the loop's seams: tests replace the wait, the jitter and the refresh.
type keepalive struct {
	wait    func(ctx context.Context, d time.Duration) bool // false when ctx ended first
	jitter  func(max time.Duration) time.Duration
	refresh func(ctx context.Context, p *providers.Provider) (bool, error)
	timeout time.Duration // bounds each refresh call
}

func newKeepalive() keepalive {
	return keepalive{
		wait: func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return true
			case <-ctx.Done():
				return false
			}
		},
		jitter:  func(max time.Duration) time.Duration { return rand.N(max) },
		refresh: providers.Keepalive,
		timeout: keepaliveTimeout,
	}
}

// touch records that a request just used the named profiles.
func (r *Router) touch(names ...string) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		r.used[n] = now
	}
}

// recentlyUsed is the sorted profiles used within keepaliveWindow; older entries are dropped.
func (r *Router) recentlyUsed() []string {
	cutoff := r.now().Add(-keepaliveWindow)
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for n, at := range r.used {
		if at.Before(cutoff) {
			delete(r.used, n)
			continue
		}
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// startKeepalive runs the loop until ctx ends or the returned stop is called; stop waits for the loop to exit.
func (r *Router) startKeepalive(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for r.ka.wait(ctx, keepaliveEvery+r.ka.jitter(keepaliveJitter)) {
			r.keepaliveTick(ctx)
		}
	}()
	return func() { cancel(); <-done }
}

// keepaliveTick refreshes each recently used profile in turn, one bounded call apiece.
func (r *Router) keepaliveTick(ctx context.Context) {
	names := r.recentlyUsed()
	if len(names) == 0 {
		return
	}
	reg, err := r.registry()
	if err != nil {
		return // requests report a broken registry
	}
	for _, n := range names {
		if p := reg.Providers[n]; p != nil && ctx.Err() == nil {
			r.keepaliveOne(ctx, p)
		}
	}
}

// keepaliveOne logs the profile name and the failure's verdict only: an error text may carry a token.
func (r *Router) keepaliveOne(ctx context.Context, p *providers.Provider) {
	defer func() {
		if recover() != nil {
			fmt.Fprintf(r.log, "proxy: keepalive %s: failed (panic)\n", p.Name)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, r.ka.timeout)
	defer cancel()
	refreshed, err := r.ka.refresh(ctx, p)
	switch {
	case err != nil:
		fmt.Fprintf(r.log, "proxy: keepalive %s: failed (%s)\n", p.Name, providers.Verdict(err))
	case refreshed:
		fmt.Fprintf(r.log, "proxy: keepalive %s: refreshed\n", p.Name)
	}
}
