package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/usage"
)

//go:generate sh -c "mkdir -p assets/usage && cd ../../../vscode/claude-code-agents/lib && cp format.js pricing.js usage-model.js usage-view.js ../../../internal/network/web/assets/usage/"

const (
	usageTTL      = 30 * time.Second // a cold build scans transcripts and calls quota endpoints
	usageFreshMin = 5 * time.Second  // ?fresh=1 skips the cache only when the entry is older than this
	maxUsageDays  = 365
)

var errUsageBuildAborted = errors.New("usage: build aborted")

// usageReport is the build seam; production is the exact function behind `claude-code usage --format json`.
var usageReport = usage.Report

// usageCache holds the last JSON body per day-window; one build per key at a time, never under the lock.
type usageCache struct {
	mu      sync.Mutex
	entries map[usageKey]usageEntry
	flights map[usageKey]*usageFlight
}

// usageFlight is an in-progress build that concurrent callers of the same key wait on.
type usageFlight struct {
	done chan struct{}
	err  error
}

// usageKey separates the quota-less body from the full one for the same window.
type usageKey struct {
	days   int
	quotas bool
}

type usageEntry struct {
	body []byte
	at   time.Time
}

// get returns the cached body for key, or builds it with build when missing, stale, or (fresh and old enough).
func (c *usageCache) get(key usageKey, fresh bool, now func() time.Time, build func() ([]byte, error)) ([]byte, error) {
	for {
		body, hit, wait, own := c.acquire(key, fresh, now)
		if hit {
			return body, nil
		}
		if own != nil {
			return c.runBuild(key, own, now, build)
		}
		<-wait.done
		if wait.err != nil {
			return nil, wait.err
		}
	}
}

// acquire returns a usable cached body (hit), a flight to wait on, or a new flight the caller must build.
func (c *usageCache) acquire(key usageKey, fresh bool, now func() time.Time) (body []byte, hit bool, wait, own *usageFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		age := now().Sub(e.at)
		if age < usageTTL && (!fresh || age < usageFreshMin) {
			return e.body, true, nil, nil
		}
	}
	if f, building := c.flights[key]; building {
		return nil, false, f, nil
	}
	f := &usageFlight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = map[usageKey]*usageFlight{}
	}
	c.flights[key] = f
	return nil, false, nil, f
}

// runBuild runs build outside the lock, caches a successful body and releases the flight's waiters, even on panic.
func (c *usageCache) runBuild(key usageKey, f *usageFlight, now func() time.Time, build func() ([]byte, error)) (body []byte, err error) {
	err = errUsageBuildAborted
	defer func() {
		c.mu.Lock()
		delete(c.flights, key)
		if err == nil {
			if c.entries == nil {
				c.entries = map[usageKey]usageEntry{}
			}
			c.entries[key] = usageEntry{body: body, at: now()}
		}
		f.err = err
		c.mu.Unlock()
		close(f.done)
	}()
	body, err = build()
	return body, err
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)

// maskUsageString turns every email into first char + "…@" + domain and redacts token-shaped runs.
func maskUsageString(s string) string {
	s = emailRe.ReplaceAllStringFunc(s, func(e string) string {
		at := strings.IndexByte(e, '@')
		r := []rune(e[:at])
		return string(r[:1]) + "…" + e[at:]
	})
	return shapes.Redact(s)
}

// maskUsageValue returns v (a decoded JSON tree) with maskUsageString applied to every string value.
func maskUsageValue(v any) any {
	switch t := v.(type) {
	case string:
		return maskUsageString(t)
	case []any:
		for i := range t {
			t[i] = maskUsageValue(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = maskUsageValue(t[k])
		}
	}
	return v
}

// maskedUsageJSON is the served body: the document's JSON with emails and token-shaped strings masked.
func maskedUsageJSON(doc *usage.Document) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := helptext.WriteJSON(&buf, maskUsageValue(tree)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleUsage serves /api/usage?days=N[&fresh=1][&quotas=0]: the `usage --format json` report, masked.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	days := 1
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxUsageDays {
			http.Error(w, "days must be 1.."+strconv.Itoa(maxUsageDays), http.StatusBadRequest)
			return
		}
		days = n
	}
	quotas := r.URL.Query().Get("quotas") != "0"
	// a cold 30-day build took ~29s in measurement, right at the server's 30s WriteTimeout
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
	body, err := s.usage.get(usageKey{days, quotas}, r.URL.Query().Get("fresh") == "1", s.now, func() ([]byte, error) {
		doc, err := usageReport(s.opts.ClaudeDir, "", days, quotas, io.Discard)
		if err != nil {
			return nil, err
		}
		return maskedUsageJSON(doc)
	})
	if err != nil {
		s.logger.Printf("usage: %v", err)
		http.Error(w, "usage unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleUsageAsset serves one embedded copy of a shared vscode/claude-code-agents/lib module.
func (s *Server) handleUsageAsset(w http.ResponseWriter, r *http.Request) {
	body, err := assetsFS.ReadFile("assets/usage/" + path.Base(r.PathValue("name")))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
