package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tree"
)

// Hub timings and the Build seam are vars so tests can shorten the intervals and count builds.
var (
	scanInterval  = time.Second      // how often the scanner computes the cheap fingerprint
	scanHeartbeat = 30 * time.Second // forced rebuild: catches pid deaths and clock-driven status changes
	ssePing       = 20 * time.Second // SSE comment keep-alive so idle proxies keep the stream open
	buildTree     = tree.Build       // the one full scan; a seam for counting builds
	sseWriteLimit = 10 * time.Second // per-frame write deadline: a client that stops reading is dropped
)

// hub is the single tree scanner shared by every /api/events subscriber and /api/tree hit.
type hub struct {
	build       func() ([]byte, error) // full rebuild, marshalled exactly as writeJSON would
	fingerprint func() uint64          // cheap change detector over what build reads
	logf        func(format string, args ...any)
	done        chan struct{} // closed when the scanner exits, so SSE handlers return on shutdown

	mu   sync.Mutex
	subs map[chan []byte]struct{}
	last []byte
	fp   uint64
}

func newHub(build func() ([]byte, error), fingerprint func() uint64, logf func(string, ...any)) *hub {
	return &hub{
		build: build, fingerprint: fingerprint, logf: logf,
		done: make(chan struct{}), subs: map[chan []byte]struct{}{},
	}
}

// subscribe registers a buffered-1 wake channel and returns the current payload (nil before the first scan).
func (h *hub) subscribe() (chan []byte, []byte) {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	return ch, h.last
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// latest returns the newest payload, nil before the first scan.
func (h *hub) latest() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// publish stores payload and wakes every subscriber; identical bytes are a no-op, a full wake buffer is skipped.
func (h *hub) publish(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if bytes.Equal(payload, h.last) {
		return
	}
	h.last = payload
	for ch := range h.subs {
		select {
		case ch <- payload:
		default:
		}
	}
}

// rebuild builds and publishes; fp was taken before the build so a change mid-build is caught next tick.
func (h *hub) rebuild(fp uint64) {
	payload, err := h.build()
	h.mu.Lock()
	h.fp = fp
	h.mu.Unlock()
	if err != nil {
		h.logf("web: tree scan: %v", err)
		return
	}
	h.publish(payload)
}

// run scans once, then every scanInterval rebuilds on a fingerprint change or scanHeartbeat; returns on ctx.Done.
func (h *hub) run(ctx context.Context) {
	defer close(h.done)
	h.rebuild(h.fingerprint())
	lastBuild := time.Now()
	tick := time.NewTicker(scanInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		fp := h.fingerprint()
		h.mu.Lock()
		same := h.fp == fp
		h.mu.Unlock()
		if same && time.Since(lastBuild) < scanHeartbeat {
			continue
		}
		h.rebuild(fp)
		lastBuild = time.Now()
	}
}

// treeHub returns the server's hub, created on first use so struct-literal Servers need no setup.
func (s *Server) treeHub() *hub {
	s.hubOnce.Do(func() { s.hub = newHub(s.treeJSON, s.treeFingerprint, s.logger.Printf) })
	return s.hub
}

// runHub runs the shared scanner until ctx is done; Run starts it and tests start it explicitly.
func (s *Server) runHub(ctx context.Context) { s.treeHub().run(ctx) }

func (s *Server) treeDays() int {
	if s.opts.Days <= 0 {
		return 1
	}
	return s.opts.Days
}

// treeJSON is Build plus the default idle filter, marshalled the way writeJSON does (trailing newline included).
func (s *Server) treeJSON() ([]byte, error) {
	snap, err := buildTree(s.opts.ClaudeDir, tree.Options{Days: s.treeDays()})
	if err != nil {
		return nil, err
	}
	snap.Sessions = tree.FilterIdleSessions(snap.Sessions)
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(snap); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// treeFingerprint hashes name, mtime and size of every file tree.Build reads: sessions plus dispatch records.
func (s *Server) treeFingerprint() uint64 {
	root := s.opts.ClaudeDir
	cutoff := time.Now().Add(-time.Duration(s.treeDays()) * 24 * time.Hour)
	h := fnv.New64a()
	add := func(path string) (os.FileInfo, bool) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false
		}
		var b [16]byte
		binary.LittleEndian.PutUint64(b[:8], uint64(info.ModTime().UnixNano()))
		binary.LittleEndian.PutUint64(b[8:], uint64(info.Size()))
		_, _ = h.Write([]byte(path))
		_, _ = h.Write(b[:])
		return info, true
	}
	mains, _ := filepath.Glob(filepath.Join(root, "projects", "*", "*.jsonl"))
	for _, f := range mains {
		if info, ok := add(f); !ok || info.ModTime().Before(cutoff) {
			continue
		}
		dir := f[:len(f)-len(".jsonl")]
		for _, pat := range []string{"subagents/*", "subagents/workflows/*/*", "workflows/*"} {
			matches, _ := filepath.Glob(filepath.Join(dir, pat))
			for _, m := range matches {
				add(m)
			}
		}
	}
	regs, _ := filepath.Glob(filepath.Join(root, "sessions", "*.json"))
	for _, f := range regs {
		add(f)
	}
	recs, _ := filepath.Glob(filepath.Join(agent.DispatchDir(), "*.json")) // buildDispatches reads these records
	for _, f := range recs {
		add(f)
	}
	return h.Sum64()
}

// handleEvents serves /api/events: the current tree at once, then one `tree` frame per change.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	_, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "web: streaming unsupported", http.StatusInternalServerError)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{}) // the server read timeout would otherwise cut the stream; errors only on fakes
	h := s.treeHub()
	ch, cur := h.subscribe()
	defer h.unsubscribe(ch)
	ph := s.plansHub()
	pch, pcur := ph.subscribe()
	defer ph.unsubscribe(pch)
	if cur == nil {
		var err error
		if cur, err = s.treeJSON(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	if pcur == nil {
		var err error
		if pcur, err = s.plansJSON(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	sent, psent := cur, pcur
	if !writeTreeFrame(rc, w, cur) || !writeEventFrame(rc, w, "plans", pcur) {
		return
	}
	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-ph.done:
			return
		case <-ping.C:
			if !writeFlush(rc, w, []byte(": ping\n\n")) {
				return
			}
		case <-pch:
			next := ph.latest()
			if bytes.Equal(next, psent) {
				continue
			}
			psent = next
			if !writeEventFrame(rc, w, "plans", next) {
				return
			}
		case <-ch:
			next := h.latest()
			if bytes.Equal(next, sent) {
				continue
			}
			sent = next
			if !writeTreeFrame(rc, w, next) {
				return
			}
		}
	}
}

// writeFlush writes b under a fresh bounded write deadline and flushes; false means drop the client.
func writeFlush(rc *http.ResponseController, w http.ResponseWriter, b []byte) bool {
	if err := rc.SetWriteDeadline(time.Now().Add(sseWriteLimit)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return false
	}
	if _, err := w.Write(b); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// writeTreeFrame writes one `event: tree` frame (JSON is single-line, so one data: line) and flushes.
func writeTreeFrame(rc *http.ResponseController, w http.ResponseWriter, payload []byte) bool {
	return writeEventFrame(rc, w, "tree", payload)
}

// writeEventFrame writes one named SSE frame and flushes.
func writeEventFrame(rc *http.ResponseController, w http.ResponseWriter, name string, payload []byte) bool {
	frame := append(append([]byte("event: "+name+"\ndata: "), bytes.TrimRight(payload, "\n")...), '\n', '\n')
	return writeFlush(rc, w, frame)
}
