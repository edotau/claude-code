// Package web serves `claude-code network page`: a small, phone-friendly page over the tailnet for watching
// agent sessions/subagents and dispatching new ones, reusing internal/agenttree and internal/dispatch.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
)

// Options configures Run.
type Options struct {
	Addr           string    // "" resolves this node's tailnet IPv4 address on DefaultPort
	Hosts          []string  // allowed dispatch hosts; empty uses the tailnet machine list
	Days           int       // agent-tree scope in days, forwarded to tree.Options
	DryRunDispatch bool      // POST /api/dispatch returns the ssh argv instead of running it
	ClaudeDir      string    // "" defaults to paths.ConfigDir()
	ErrOut         io.Writer // nil discards
}

// Server is the bound web dashboard: its CSRF token and host allowlist are fixed for the process.
type Server struct {
	opts       Options
	boundAddr  string
	dnsNames   []string
	csrfToken  string
	logger     *log.Logger
	mux        *http.ServeMux
	run        tailscale.RemoteRunner // executes a dispatch capture/list command; tests fake this
	now        func() time.Time
	status     *statusCache  // expiring, synchronized pane-status cache, keyed by host
	browserEnv browser.Env   // the detection seam; tests fake its Run/Home/AppSupport
	browsers   *browserCache // expiring, synchronized browser.Detect cache
	usage      usageCache    // short-lived /api/usage bodies, keyed by day window
	hosts      hostsCache    // expiring tailnet machine-name list for resolveHosts
	hubOnce    sync.Once     // lazily creates hub, so struct-literal Servers need no setup
	hub        *hub          // the single shared tree scanner behind /api/events and /api/tree
	plansOnce  sync.Once     // lazily creates plans
	plans      *hub          // the plans/ scanner behind the `plans` SSE frames
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
		run:        tailscale.ExecRunner,
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
	plansDone := make(chan struct{})
	go func() { defer close(plansDone); s.plansHub().run(hctx) }()
	defer func() { stopHub(); <-hubDone; <-plansDone }()
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
	s.mux.HandleFunc("GET /api/plans", s.guard(s.handlePlans))
	s.mux.HandleFunc("GET /api/plan", s.guard(s.handlePlan))
	s.mux.HandleFunc("GET /api/dispatches", s.guard(s.handleDispatches))
	s.mux.HandleFunc("POST /api/dispatch", s.guard(s.handleDispatch))
	s.mux.HandleFunc("GET /api/view", s.guard(s.handleView))
	s.mux.HandleFunc("GET /api/browsers", s.guard(s.handleBrowsers))
	s.mux.HandleFunc("GET /api/usage", s.guard(s.handleUsage))
	s.mux.HandleFunc("GET /assets/usage/{name}", s.guard(s.handleUsageAsset))
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

// DefaultPort is the port `claude-code network page` binds by default.
const DefaultPort = 7777

// cgnat is the 100.64.0.0/10 Carrier-Grade-NAT block tailscale assigns IPv4 node addresses from.
var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isCGNAT reports whether ip is a tailnet IPv4 address (100.64.0.0/10).
func isCGNAT(ip net.IP) bool { return ip != nil && cgnat.Contains(ip) }

// defaultAddr resolves this machine's tailscale IPv4 address on DefaultPort; note is a printable
// explanation set only on the loopback fallback (tailscale down, not installed, or no IPv4 found).
func defaultAddr(ctx context.Context) (addr, note string) {
	ip, err := tailscaleIPv4(ctx)
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultPort))
	if err != nil {
		return loopback, fmt.Sprintf("web: no tailnet IPv4 address (%v); binding loopback instead", err)
	}
	return net.JoinHostPort(ip, strconv.Itoa(DefaultPort)), ""
}

// tailscaleIPv4 returns this node's tailnet (100.64.0.0/10) IPv4 address from `tailscale status`.
func tailscaleIPv4(ctx context.Context) (string, error) {
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return "", err
	}
	for _, s := range st.IPs {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil && isCGNAT(ip) {
			return s, nil
		}
	}
	return "", errors.New("tailscale has no IPv4 address for this node")
}

// validateBindAddr parses addr as host:port and refuses anything but a loopback or tailnet
// (100.64.0.0/10) literal IP — never 0.0.0.0, ::, an empty host, or a hostname.
func validateBindAddr(addr string) (net.IP, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web: --addr %q: %w", addr, err)
	}
	if host == "" {
		return nil, fmt.Errorf("web: --addr %q: binding every interface is refused", addr)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("web: --addr %q: bad port: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("web: --addr %q: host must be a literal IP, not a name", addr)
	}
	if !ip.IsLoopback() && !isCGNAT(ip) {
		return nil, fmt.Errorf("web: --addr %q: refused — only a loopback or tailnet 100.64.0.0/10 address may be bound", addr)
	}
	return ip, nil
}

// tailnetNames returns this node's MagicDNS name, full and short (before the first "."), lowercased
// and with any trailing dot trimmed; empty when tailscale status can't be read.
func tailnetNames(ctx context.Context) []string {
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return nil
	}
	name := strings.ToLower(strings.TrimSuffix(st.DNSName, "."))
	if name == "" {
		return nil
	}
	names := []string{name}
	if short, _, ok := strings.Cut(name, "."); ok {
		names = append(names, short)
	}
	return names
}

// csrfHeader carries the per-process token every mutating request must echo back.
const csrfHeader = "X-CSRF-Token"

// maxTaskBytes caps the dispatch task field; a phone keyboard has no business sending more.
const maxTaskBytes = 8 << 10

// maxBodyBytes caps a dispatch body: a fully percent-encoded task (3x) plus room for the other fields.
const maxBodyBytes = 3*maxTaskBytes + 4<<10

// newCSRFToken returns 32 random bytes, hex-encoded, generated once per server process.
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// constantTimeEqual compares two tokens without leaking timing information about where they differ.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hostAllowed reports whether the Host header hostHeader (as net/http presents it, already stripped
// of a leading scheme) names this server: the exact bound host:port, "localhost" (with or without a
// port), or one of this node's tailnet MagicDNS names (full or short, with or without a port) — the
// DNS-rebinding defence: an attacker's page cannot point a browser's Host header at a tailnet IP by
// registering a public DNS name that resolves there, because that name is never on this allowlist.
func (s *Server) hostAllowed(hostHeader string) bool {
	h := strings.ToLower(hostHeader)
	if h == strings.ToLower(s.boundAddr) {
		return true
	}
	hostOnly := h
	if host, _, err := net.SplitHostPort(h); err == nil {
		hostOnly = host
	}
	if hostOnly == "localhost" {
		return true
	}
	return slices.Contains(s.dnsNames, hostOnly)
}

// checkOrigin rejects a cross-origin request when the browser sent Origin or Sec-Fetch-Site: same
// trust boundary as the Host check, but the only line of defence available cross-scheme (http vs
// https) or when a browser extension spoofs Host but can't spoof these.
func (s *Server) checkOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // no browser-set origin info at all (curl, same-origin fetch omitting it): allow
	}
	return s.hostAllowed(strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://"))
}

// checkCSRF requires the per-process token on every mutating request, via header only (never a form
// field an attacker's cross-site form could also send blind).
func (s *Server) checkCSRF(r *http.Request) bool {
	return constantTimeEqual(r.Header.Get(csrfHeader), s.csrfToken)
}

//go:embed assets/index.html.tmpl assets/usage/*.js
var assetsFS embed.FS

// indexTemplate is the dashboard page: inline CSS/JS plus the shared usage modules under /assets/usage/.
var indexTemplate = sync.OnceValue(func() *template.Template { return template.Must(template.ParseFS(assetsFS, "assets/index.html.tmpl")) })
