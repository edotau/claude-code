package web

import (
	"context"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/remote"
)

// statusCacheTTL bounds how long a cached pane-status sample is served before a fresh capture
// runs: short enough to track an agent, long enough that dashboard polling doesn't hammer ssh/tmux.
const statusCacheTTL = 4 * time.Second

// hostStatus is one host's last-known pane statuses; its own lock serializes concurrent cache
// misses for that host into a single capture round trip instead of one ssh call per request.
type hostStatus struct {
	mu       sync.Mutex
	statuses []remote.PaneStatus
	sampled  time.Time
}

// statusCache holds one hostStatus per host, synchronized for concurrent requests across hosts.
type statusCache struct {
	mu     sync.Mutex
	byHost map[string]*hostStatus
}

// newStatusCache returns an empty cache.
func newStatusCache() *statusCache { return &statusCache{byHost: map[string]*hostStatus{}} }

// entry returns (creating if absent) host's hostStatus record.
func (c *statusCache) entry(host string) *hostStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.byHost[host]
	if !ok {
		h = &hostStatus{}
		c.byHost[host] = h
	}
	return h
}

// get returns host's cached pane statuses, refreshing via run when the cache has aged past
// statusCacheTTL. It never returns raw captures — only the classified, bounded remote.PaneStatus.
func (c *statusCache) get(ctx context.Context, host string, run remote.Runner, now time.Time) ([]remote.PaneStatus, error) {
	h := c.entry(host)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sampled.IsZero() && now.Sub(h.sampled) < statusCacheTTL {
		return h.statuses, nil
	}
	caps, err := remote.CapturePanes(ctx, host, true, run)
	h.statuses = remote.Observe(h.statuses, caps, err, now)
	h.sampled = now
	if err != nil && len(h.statuses) == 0 {
		return nil, err
	}
	return h.statuses, nil
}
