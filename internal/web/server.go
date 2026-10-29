// Package web serves `claude-code network page`: a small, phone-friendly page over the tailnet for watching
// agent sessions/subagents and dispatching new ones, reusing internal/agenttree and internal/dispatch.
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/dispatch"
	"github.com/edotau/claude-code/internal/paths"
)

// Options configures Run.
type Options struct {
	Addr           string    // "" resolves this node's tailnet IPv4 address on DefaultPort
	Hosts          []string  // allowed dispatch hosts; empty uses the tailnet machine list
	Days           int       // agent-tree scope in days, forwarded to agenttree.Options
	DryRunDispatch bool      // POST /api/dispatch returns the ssh argv instead of running it
	ClaudeDir      string    // "" defaults to paths.ConfigDir()
	Out, ErrOut    io.Writer // "" defaults to os.Stdout/os.Stderr; tests redirect these
}

// Server is the bound web dashboard: its CSRF token and host allowlist are fixed for the process.
type Server struct {
	opts       Options
	boundAddr  string
	dnsNames   []string
	csrfToken  string
	logger     *log.Logger
	mux        *http.ServeMux
	run        dispatch.Runner // executes a dispatch capture/list command; tests fake this
	now        func() time.Time
	status     *statusCache  // expiring, synchronized pane-status cache, keyed by host
	browserEnv browser.Env   // the detection seam; tests fake its Run/Home/AppSupport
	browsers   *browserCache // expiring, synchronized browser.Detect cache
	hosts      hostsCache    // expiring tailnet machine-name list for resolveHosts
	hubOnce    sync.Once     // lazily creates hub, so struct-literal Servers need no setup
	hub        *hub          // the single shared tree scanner behind /api/events and /api/tree
}

// New builds a Server for opts.Addr (or the resolved default) without binding a listener; callers
// that only want to construct routes/handlers for a test use this directly.
func New(ctx context.Context, opts Options) (*Server, error) {
	addr := opts.Addr
	note := ""
	if addr == "" {
		addr, note = defaultAddr(ctx)
	}
	if _, err := validateBindAddr(addr); err != nil {
		return nil, err
	}
	if opts.ClaudeDir == "" {
		opts.ClaudeDir = paths.ConfigDir()
	}
	token, err := newCSRFToken()
	if err != nil {
		return nil, fmt.Errorf("web: generating CSRF token: %w", err)
	}
	errw := opts.ErrOut
	if errw == nil {
		errw = io.Discard
	}
	s := &Server{
		opts:       opts,
		boundAddr:  addr,
		dnsNames:   tailnetNames(ctx),
		csrfToken:  token,
		logger:     log.New(errw, "", log.LstdFlags),
		run:        dispatch.ExecRunner,
		now:        time.Now,
		status:     newStatusCache(),
		browserEnv: browser.NewEnv(),
		browsers:   newBrowserCache(),
	}
	if note != "" {
		s.logger.Print(note)
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s, nil
}

// Run resolves the bind address, listens, and serves until ctx is done, then drains in-flight
// requests briefly before returning. Refuses anything but a loopback or tailnet (100.64.0.0/10) addr.
func Run(ctx context.Context, opts Options) error {
	s, err := New(ctx, opts)
	if err != nil {
		return err
	}
	hctx, stopHub := context.WithCancel(ctx)
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); s.runHub(hctx) }()
	defer func() { stopHub(); <-hubDone }()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.boundAddr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.boundAddr, err)
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	srv.RegisterOnShutdown(stopHub) // hub exit closes h.done, which ends every open SSE stream
	s.logger.Printf("web: listening on http://%s", s.boundAddr)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(dctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// routes registers the dashboard's handlers, wrapped by the Host/Origin guard and access logging.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.guard(s.handleIndex))
	s.mux.HandleFunc("GET /api/tree", s.guard(s.handleTree))
	s.mux.HandleFunc("GET /api/events", s.guard(s.handleEvents))
	s.mux.HandleFunc("GET /api/dispatches", s.guard(s.handleDispatches))
	s.mux.HandleFunc("POST /api/dispatch", s.guard(s.handleDispatch))
	s.mux.HandleFunc("GET /api/view", s.guard(s.handleView))
	s.mux.HandleFunc("GET /api/browsers", s.guard(s.handleBrowsers))
}

// guard wraps h with the Host-header/Origin DNS-rebinding defence and a one-line access log; a
// rejected request never reaches the handler and its body is never logged.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			s.logger.Printf("%s %s host=%q -> 421", r.Method, r.URL.Path, r.Host)
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		if !s.checkOrigin(r) {
			s.logger.Printf("%s %s origin=%q -> 403", r.Method, r.URL.Path, r.Header.Get("Origin"))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		s.logger.Printf("%s %s", r.Method, r.URL.Path)
		h(w, r)
	}
}
