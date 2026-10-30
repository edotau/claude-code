package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/browser"
)

// browserCacheTTL bounds how long a cached browser report is served before browser.Detect runs
// again: Detect shells out to mdfind/plutil/defaults per browser, so every request must not re-probe.
const browserCacheTTL = 30 * time.Second

// browserCache holds the one most-recent browser.Report, synchronized for concurrent requests.
type browserCache struct {
	mu      sync.Mutex
	report  browser.Report
	sampled time.Time
}

func newBrowserCache() *browserCache { return &browserCache{} }

// get returns the cached report, refreshing via browser.Detect(env) once sampled has aged past
// browserCacheTTL.
func (c *browserCache) get(env browser.Env, now time.Time) browser.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sampled.IsZero() && now.Sub(c.sampled) < browserCacheTTL {
		return c.report
	}
	c.report = browser.Detect(env)
	c.sampled = now
	return c.report
}

// handleBrowsers serves /api/browsers: the cached browser.Detect report (installed browsers, the
// system default, and each one's control-path state), read-only.
func (s *Server) handleBrowsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.browsers.get(s.browserEnv, s.now()))
}
