package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agenttree"
	"github.com/edotau/claude-code/internal/dispatch"
	"github.com/edotau/claude-code/internal/tailscale"
)

// sshTimeout bounds one ssh round trip (dispatch start or list) from the web server.
const sshTimeout = 20 * time.Second

// pageData feeds the index template: everything it needs to render without a second round trip.
type pageData struct {
	CSRFToken string
	Hosts     []string
	Agents    []string
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	data := pageData{CSRFToken: s.csrfToken, Hosts: s.resolveHosts(context.Background()), Agents: dispatch.Agents}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		s.logger.Printf("render index: %v", err)
	}
}

// handleTree serves /api/tree: the same Build + default idle filter as `claude-code agents tree`.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	days := s.opts.Days
	if days <= 0 {
		days = 1
	}
	tree, err := agenttree.Build(s.opts.ClaudeDir, agenttree.Options{Days: days})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tree.Sessions = agenttree.FilterIdleSessions(tree.Sessions)
	writeJSON(w, tree)
}

// handleDispatches serves /api/dispatches?host=H: the cc-* tmux sessions on an allowed host.
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
	argv := dispatch.SSHArgs(resolved, dispatch.ListCommand())
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, dispatch.ParseSessions(string(out)))
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
	if !dispatch.ValidAgent(req.Agent) {
		http.Error(w, "web: --agent must be one of "+strings.Join(dispatch.Agents, "|"), http.StatusBadRequest)
		return
	}
	if len(req.Task) > maxTaskBytes {
		http.Error(w, "web: task exceeds 8KiB", http.StatusBadRequest)
		return
	}
	remoteCmd, session, err := dispatch.BuildRemoteCommand(dispatch.Opts{Dir: req.Dir, Agent: req.Agent, Task: req.Task})
	if err != nil {
		http.Error(w, "web: "+err.Error(), http.StatusBadRequest)
		return
	}
	argv := dispatch.SSHArgs(resolved, remoteCmd)
	if s.opts.DryRunDispatch {
		writeJSON(w, dispatchResponse{Session: session, Attach: dispatch.AttachCommand(resolved, session), Argv: argv})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sshTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		s.logger.Printf("dispatch %s@%s: %v: %s", req.Agent, resolved, err, strings.TrimSpace(string(out)))
		http.Error(w, "web: dispatch failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, dispatchResponse{Session: session, Attach: dispatch.AttachCommand(resolved, session)})
}

// parseDispatchRequest reads either an application/json body or an application/x-www-form-urlencoded
// one; anything else is refused, matching the "form or JSON only" POST contract.
func parseDispatchRequest(r *http.Request) (dispatchRequest, error) {
	var req dispatchRequest
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		body := io.LimitReader(r.Body, maxTaskBytes+4<<10)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
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

// resolveHosts is s.opts.Hosts verbatim when set, else every tailnet machine name (best-effort: a
// tailscale error just yields an empty list, not a failed page load).
func (s *Server) resolveHosts(ctx context.Context) []string {
	if len(s.opts.Hosts) > 0 {
		return s.opts.Hosts
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
	return hosts
}

// allowedHost reports whether host (full or short name) is in the dispatch allowlist, resolving it
// to the form dispatch.SSHArgs should use. "localhost" is always allowed, matching dispatch's own rule.
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
	errUnsupportedContentType = jsonErr("web: Content-Type must be application/json or application/x-www-form-urlencoded")
	errMissingField           = jsonErr("web: host, agent and task are required")
)

type jsonErr string

func (e jsonErr) Error() string { return string(e) }
