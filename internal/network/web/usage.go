package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/usage"
)

//go:generate sh -c "mkdir -p assets/usage && cd ../../../vscode/claude-code-agents/lib && cp format.js pricing.js usage-model.js usage-view.js ../../../internal/network/web/assets/usage/"

const (
	usageTTL      = 30 * time.Second // a cold build scans transcripts and calls quota endpoints
	usageFreshMin = 5 * time.Second  // ?fresh=1 skips the cache only when the entry is older than this
	maxUsageDays  = 365
)

// usageReport is the build seam; production is the exact function behind `claude-code usage --format json`.
var usageReport = usage.Report

// usageCache holds the last JSON body per day-window; the lock spans the build so concurrent callers coalesce.
type usageCache struct {
	mu      sync.Mutex
	entries map[int]usageEntry
}

type usageEntry struct {
	body []byte
	at   time.Time
}

// get returns the cached body for days, or builds it with build when missing, stale, or (fresh and old enough).
func (c *usageCache) get(days int, fresh bool, now time.Time, build func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[days]; ok {
		age := now.Sub(e.at)
		if age < usageTTL && !(fresh && age >= usageFreshMin) {
			return e.body, nil
		}
	}
	body, err := build()
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = map[int]usageEntry{}
	}
	c.entries[days] = usageEntry{body: body, at: now}
	return body, nil
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

// maskedUsageJSON is the served body: the document's JSON with emails and token-shaped strings masked (CLI output is unmasked).
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

// handleUsage serves /api/usage?days=N[&fresh=1]: `claude-code usage --days N --format json` with emails and token-shaped strings masked.
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
	// a cold 30-day build took ~29s in measurement, right at the server's 30s WriteTimeout
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
	body, err := s.usage.get(days, r.URL.Query().Get("fresh") == "1", s.now(), func() ([]byte, error) {
		doc, err := usageReport(s.opts.ClaudeDir, "", days, true, io.Discard)
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
