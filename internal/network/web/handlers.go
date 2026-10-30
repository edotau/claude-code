package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/network/tailscale"
)

// sshTimeout bounds one ssh round trip (dispatch start or list) from the web server.
const sshTimeout = 20 * time.Second

// pageData feeds the index template: everything it needs to render without a second round trip.
type pageData struct {
	CSRFToken string
	Hosts     []string
	Agents    []string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := pageData{CSRFToken: s.csrfToken, Hosts: s.resolveHosts(r.Context()), Agents: tailscale.RemoteAgents}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate().Execute(w, data); err != nil {
		s.logger.Printf("render index: %v", err)
	}
}

// handleTree serves /api/tree: the hub's cached Build + default idle filter, or a direct Build before the first scan.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	payload := s.treeHub().latest()
	if payload == nil {
		var err error
		if payload, err = s.treeJSON(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(payload)
}

// handleDispatches serves /api/dispatches?host=H: classified pane status on an allowed host,
// backed by Server's expiring cache. It exposes status only — never a pane's raw captured text.
func (s *Server) handleDispatches(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		http.Error(w, "web: ?host is required", http.StatusBadRequest)
		return
	}
	resolved, ok := s.allowedHost(r.Context(), host)
	if !ok {
		http.Error(w, "web: unknown host", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sshTimeout)
	defer cancel()
	statuses, err := s.status.get(ctx, resolved, s.run, s.now())
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, statuses)
}

// dispatchRequest is the POST /api/dispatch body (form or JSON).
type dispatchRequest struct {
	Host  string `json:"host"`
	Agent string `json:"agent"`
	Dir   string `json:"dir"`
	Task  string `json:"task"`
}

// dispatchResponse is what POST /api/dispatch returns on success.
type dispatchResponse struct {
	Session string   `json:"session"`
	Attach  string   `json:"attach"`
	Argv    []string `json:"argv,omitempty"` // only set under --dry-run-dispatch
}

func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Error(w, "web: missing or bad "+csrfHeader, http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes) // bounds the form and multipart paths too
	req, err := parseDispatchRequest(r)
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusBadRequest)
		return
	}
	resolved, ok := s.allowedHost(r.Context(), req.Host)
	if !ok {
		http.Error(w, "web: unknown host", http.StatusBadRequest)
		return
	}
	if !tailscale.ValidAgent(req.Agent) {
		http.Error(w, "web: --agent must be one of "+strings.Join(tailscale.RemoteAgents, "|"), http.StatusBadRequest)
		return
	}
	if len(req.Task) > maxTaskBytes {
		http.Error(w, "web: task exceeds 8KiB", http.StatusBadRequest)
		return
	}
	remoteCmd, session, err := tailscale.BuildRemoteCommand(tailscale.RemoteOpts{Dir: req.Dir, Agent: req.Agent, Task: req.Task})
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusBadRequest)
		return
	}
	argv := tailscale.ExecArgs(r.Context(), resolved, remoteCmd, true)
	if s.opts.DryRunDispatch {
		writeJSON(w, dispatchResponse{Session: session, Attach: tailscale.AttachCommand(resolved, session), Argv: argv})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sshTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		s.logger.Printf("dispatch %s@%s: %v: %s", req.Agent, resolved, err, strings.TrimSpace(string(out)))
		http.Error(w, "web: dispatch failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, dispatchResponse{Session: session, Attach: tailscale.AttachCommand(resolved, session)})
}

// parseDispatchRequest reads either an application/json body or an application/x-www-form-urlencoded
// one; anything else is refused, matching the "form or JSON only" POST contract.
func parseDispatchRequest(r *http.Request) (dispatchRequest, error) {
	var req dispatchRequest
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, err
		}
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"), strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseForm(); err != nil {
			return req, err
		}
		req = dispatchRequest{Host: r.FormValue("host"), Agent: r.FormValue("agent"), Dir: r.FormValue("dir"), Task: r.FormValue("task")}
	default:
		return req, errUnsupportedContentType
	}
	if req.Host == "" || req.Agent == "" || req.Task == "" {
		return req, errMissingField
	}
	return req, nil
}

// hostsCacheTTL bounds reuse of the tailnet machine list: each refresh is an admin-API call plus `tailscale status`.
const hostsCacheTTL = 30 * time.Second

// hostsCache holds the last successful tailnet machine-name list; its zero value is an empty cache.
type hostsCache struct {
	mu      sync.Mutex
	hosts   []string
	sampled time.Time
}

// resolveHosts is s.opts.Hosts verbatim when set, else every tailnet machine name, cached for
// hostsCacheTTL (best-effort: a tailscale error yields an empty, uncached list, not a failed page load).
func (s *Server) resolveHosts(ctx context.Context) []string {
	if len(s.opts.Hosts) > 0 {
		return s.opts.Hosts
	}
	c := &s.hosts
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if !c.sampled.IsZero() && now.Sub(c.sampled) < hostsCacheTTL {
		return c.hosts
	}
	machines, err := tailscale.Machines(ctx)
	if err != nil {
		return nil
	}
	hosts := make([]string, 0, len(machines))
	for _, m := range machines {
		if m.Name != "" {
			hosts = append(hosts, m.Name)
		}
	}
	c.hosts, c.sampled = hosts, now
	return hosts
}

// allowedHost reports whether host (full or short name) is in the dispatch allowlist, resolving it
// to the form tailscale.SSHArgs should use. "localhost" is always allowed, matching dispatch's own rule.
func (s *Server) allowedHost(ctx context.Context, host string) (string, bool) {
	if host == "localhost" {
		return host, true
	}
	for _, h := range s.resolveHosts(ctx) {
		if strings.EqualFold(h, host) {
			return h, true
		}
		if short, _, ok := strings.Cut(h, "."); ok && strings.EqualFold(short, host) {
			return h, true
		}
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

var (
	errUnsupportedContentType = errors.New("web: Content-Type must be application/json or application/x-www-form-urlencoded")
	errMissingField           = errors.New("web: host, agent and task are required")
)

// statusCacheTTL bounds how long a cached pane-status sample is served before a fresh capture
// runs: short enough to track an agent, long enough that dashboard polling doesn't hammer ssh/tmux.
const statusCacheTTL = 4 * time.Second

// hostStatus is one host's last-known pane statuses; its own lock serializes concurrent cache
// misses for that host into a single capture round trip instead of one ssh call per request.
type hostStatus struct {
	mu       sync.Mutex
	statuses []tailscale.PaneStatus
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
// statusCacheTTL. It never returns raw captures — only the classified, bounded tailscale.PaneStatus.
func (c *statusCache) get(ctx context.Context, host string, run tailscale.RemoteRunner, now time.Time) ([]tailscale.PaneStatus, error) {
	h := c.entry(host)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sampled.IsZero() && now.Sub(h.sampled) < statusCacheTTL {
		return h.statuses, nil
	}
	caps, err := tailscale.CapturePanes(ctx, host, true, run)
	h.statuses = tailscale.ObservePanes(h.statuses, caps, err, now)
	h.sampled = now
	if err != nil && len(h.statuses) == 0 {
		return nil, err
	}
	return h.statuses, nil
}

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
