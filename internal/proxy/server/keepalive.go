package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

type profileUse struct {
	at     time.Time
	host   string
	marked time.Time // when the on-disk marker was last written
}

const (
	keepaliveEvery   = 60 * time.Second
	keepaliveJitter  = 20 * time.Second
	keepaliveWindow  = 24 * time.Hour // a profile is kept alive this long after its last request
	keepaliveTimeout = 30 * time.Second
	// keepaliveConcurrency bounds the profiles one tick refreshes at once.
	keepaliveConcurrency = 4
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

// markerDir holds one empty file per used profile, so a restarted or other daemon sees the use.
func markerDir() string { return filepath.Join(paths.StateDir(), "proxy", "used") }

// touch records when and through which egress host a request used the named profiles.
func (r *Router) touch(ctx context.Context, names ...string) {
	now := r.now()
	host := egress.HostContext(ctx)
	var stale []string
	r.mu.Lock()
	for _, n := range names {
		use := profileUse{at: now, host: host, marked: r.used[n].marked}
		if now.Sub(use.marked) >= time.Minute {
			use.marked = now
			stale = append(stale, n)
		}
		r.used[n] = use
	}
	r.mu.Unlock()
	writeMarkers(stale, now)
}

// writeMarkers stamps each plain-named profile's marker file with now; errors are ignored.
func writeMarkers(names []string, now time.Time) {
	if len(names) == 0 {
		return
	}
	dir := markerDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	for _, n := range names {
		if n == "" || n != filepath.Base(n) || n == "." || n == ".." {
			continue
		}
		path := filepath.Join(dir, n)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			continue
		}
		_ = f.Close()
		_ = os.Chtimes(path, now, now)
	}
}

// recentlyUsed is the sorted profiles used within keepaliveWindow, in memory or by marker; older ones are dropped.
func (r *Router) recentlyUsed() []string {
	cutoff := r.now().Add(-keepaliveWindow)
	r.mu.Lock()
	var names []string
	for n, use := range r.used {
		if use.at.Before(cutoff) {
			delete(r.used, n)
			continue
		}
		names = append(names, n)
	}
	r.mu.Unlock()
	names = append(names, markedNames(cutoff)...)
	slices.Sort(names)
	return slices.Compact(names)
}

// markedNames lists marker files not older than cutoff and deletes the older ones.
func markedNames(cutoff time.Time) []string {
	dir := markerDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		names = append(names, e.Name())
	}
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

// keepaliveTick refreshes recently used profiles up to keepaliveConcurrency at once, one bounded call apiece.
func (r *Router) keepaliveTick(ctx context.Context) {
	names := r.recentlyUsed()
	if len(names) == 0 {
		return
	}
	reg, err := r.registry()
	if err != nil {
		return // requests report a broken registry
	}
	_, _ = parallel.Map(ctx, names, keepaliveConcurrency, func(ctx context.Context, n string) (struct{}, error) {
		if p := reg.Providers[n]; p != nil && ctx.Err() == nil {
			r.keepaliveOne(ctx, p) // contains its own panics
		}
		return struct{}{}, nil
	})
}

// keepaliveOne logs the profile name and the failure's verdict only: an error text may carry a token.
func (r *Router) keepaliveOne(ctx context.Context, p *providers.Provider) {
	defer func() {
		if recover() != nil {
			fmt.Fprintf(r.log, "proxy: keepalive %s: failed (panic)\n", p.Name)
		}
	}()
	r.mu.Lock()
	host := r.used[p.Name].host
	r.mu.Unlock()
	ctx = egress.WithHost(ctx, host)
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
