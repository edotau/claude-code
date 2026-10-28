package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// SessionPath is the daemon's mint endpoint (POST) and SessionRevokePath its revoke endpoint (POST); router secret only.
const (
	SessionPath       = "/internal/session"
	SessionRevokePath = "/internal/session/revoke"
)

const (
	sessionTTL     = 12 * time.Hour  // idle lifetime, renewed per accepted request while the process lives
	sessionVerify  = 2 * time.Second // how long a liveness check is trusted
	sessionMax     = 256
	sessionTokenPf = "cct_"
)

// session is one minted token: scoped to a profile, bound to a process identity.
type session struct {
	profile string
	pid     int
	start   string
	expires time.Time
	checked time.Time
}

// sessionStore keeps tokens in memory only; they die with the daemon.
type sessionStore struct {
	mu        sync.Mutex
	m         map[string]*session
	now       func() time.Time
	procStart func(pid int) (string, error) // a seam for tests: the pid's start-time identity
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{m: map[string]*session{}, now: now, procStart: proc.StartTime}
}

// mint binds a new token to pid's current identity; a missing process is an error.
func (s *sessionStore) mint(profile string, pid int) (string, error) {
	if profile == "" || strings.ContainsAny(profile, "/?#") || pid <= 0 {
		return "", errors.New("profile and pid are required")
	}
	start, err := s.procStart(pid)
	if err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := sessionTokenPf + hex.EncodeToString(b)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if !now.Before(v.expires) {
			delete(s.m, k)
		}
	}
	if len(s.m) >= sessionMax {
		return "", errors.New("too many live session tokens")
	}
	s.m[tok] = &session{profile: profile, pid: pid, start: start, expires: now.Add(sessionTTL), checked: now}
	return tok, nil
}

func (s *sessionStore) revoke(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// accept reports whether tok may reach profile; success slides the TTL, a dead or reused process drops the token.
func (s *sessionStore) accept(tok, profile string) bool {
	if !strings.HasPrefix(tok, sessionTokenPf) || profile == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var ses *session
	for k, v := range s.m { // constant-time compare, no map lookup keyed on the presented bytes
		if subtle.ConstantTimeCompare([]byte(k), []byte(tok)) == 1 {
			ses = v
		}
	}
	if ses == nil {
		return false
	}
	if !now.Before(ses.expires) || (now.Sub(ses.checked) >= sessionVerify && !s.stillBound(ses)) {
		delete(s.m, tok)
		return false
	}
	if ses.profile != profile {
		return false
	}
	ses.checked = now
	ses.expires = now.Add(sessionTTL)
	return true
}

func (s *sessionStore) stillBound(ses *session) bool {
	start, err := s.procStart(ses.pid)
	return err == nil && start == ses.start
}

// profileOfPath returns <name> of /p/<name>/..., "" for any other path.
func profileOfPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

type sessionReq struct {
	Profile string `json:"profile,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Token   string `json:"token,omitempty"`
}

type sessionResp struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
}

// serveSession handles the mint and revoke endpoints, after the caller proved the router secret.
func (r *Router) serveSession(w http.ResponseWriter, req *http.Request) {
	var in sessionReq
	if req.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "proxy: want POST with a JSON body")
		return
	}
	if req.URL.Path == SessionRevokePath {
		r.sessions.revoke(in.Token)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tok, err := r.sessions.mint(in.Profile, in.PID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mustJSON(sessionResp{Token: tok, ExpiresIn: int(sessionTTL.Seconds())}))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// MintSessionToken asks the daemon (starting it if needed) for a token scoped to profile and bound to pid.
func MintSessionToken(ctx context.Context, profile string, pid int) (string, error) {
	var out sessionResp
	if err := postSession(ctx, true, SessionPath, sessionReq{Profile: profile, PID: pid}, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// RevokeSessionToken drops token at the daemon; a daemon that is not running holds none, so that is success.
func RevokeSessionToken(ctx context.Context, token string) error {
	err := postSession(ctx, false, SessionRevokePath, sessionReq{Token: token}, nil)
	if errors.Is(err, ErrNotRunning) {
		return nil
	}
	return err
}

func postSession(ctx context.Context, start bool, path string, body sessionReq, out *sessionResp) error {
	var base string
	if start {
		b, err := Ensure(ctx)
		if err != nil {
			return err
		}
		base = b
	} else {
		st, ok := Running(ctx)
		if !ok {
			return ErrNotRunning
		}
		base = Base(st.Port)
	}
	secret, err := ClientSecret()
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("proxy %s: %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
