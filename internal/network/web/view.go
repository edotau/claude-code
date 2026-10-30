package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// handleView serves /api/view?session=&agent=[&n=][&follow=1]: the drill-down snapshot for one
// subagent, or (follow=1) its live event stream as text/plain, reusing agent.View/Follow —
// the same engine `claude-code network view` uses, so dashboard and CLI never drift.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("agent")
	if name == "" {
		http.Error(w, "web: ?agent is required", http.StatusBadRequest)
		return
	}
	opts := s.viewOptions(r, name)
	snap, err := agent.View(opts)
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("follow") != "1" {
		writeJSON(w, snap)
		return
	}
	// The server's 30s WriteTimeout would otherwise cut a live follow stream; ctx still ends it on disconnect.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Printf("view follow: clearing write deadline: %v", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush() // headers reach the client before the first event; a quiet agent would otherwise hang the fetch
	}
	fw := flushWriter{w}
	if err := agent.Follow(r.Context(), opts, fw); err != nil && r.Context().Err() == nil {
		fmt.Fprintf(fw, "error: %v\n", err)
	}
}

// viewOptions builds agent.ViewOptions from the request's query params, defaulting Days the
// same way handleTree does.
func (s *Server) viewOptions(r *http.Request, name string) agent.ViewOptions {
	days := s.opts.Days
	if days <= 0 {
		days = 1
	}
	opts := agent.ViewOptions{
		ClaudeDir: s.opts.ClaudeDir,
		Session:   r.URL.Query().Get("session"),
		Days:      days,
		Agent:     name,
		Now:       s.now,
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n > 0 {
		opts.N = n
	}
	return opts
}

// flushWriter flushes after every Write, so a streamed follow response arrives line by line
// instead of buffering until the handler returns or the response reaches its default buffer size.
type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
