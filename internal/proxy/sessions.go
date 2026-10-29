package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
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

type persistedSession struct {
	Hash    string    `json:"hash"`
	Profile string    `json:"profile"`
	PID     int       `json:"pid"`
	Start   string    `json:"start"`
	Expires time.Time `json:"expires"`
}

type sessionState struct {
	Schema   int                `json:"schema"`
	Sessions []persistedSession `json:"sessions"`
}

func sessionsFile() string { return filepath.Join(paths.StateDir(), "proxy", "sessions.json") }

func sessionHash(tok string) string {
	hash := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(hash[:])
}

// sessionStore persists only token hashes, scoped to the config directory at creation.
type sessionStore struct {
	mu        sync.Mutex
	m         map[string]*session
	file      string
	now       func() time.Time
	procStart func(pid int) (string, error) // a seam for tests: the pid's start-time identity
}

func newSessionStore(now func() time.Time) *sessionStore {
	s := &sessionStore{m: map[string]*session{}, file: sessionsFile(), now: now, procStart: proc.StartTime}
	raw, err := os.ReadFile(s.file)
	var state sessionState
	if err != nil || !schemas.DecodeOK(routerSessionsFormat, raw, &state) {
		return s
	}
	pruned := false
	for _, saved := range state.Sessions {
		hash, err := hex.DecodeString(saved.Hash)
		if err != nil || len(hash) != sha256.Size || saved.Hash != strings.ToLower(saved.Hash) ||
			saved.Profile == "" || strings.ContainsAny(saved.Profile, "/?#") || saved.PID <= 0 || saved.Start == "" ||
			!now().Before(saved.Expires) {
			pruned = true
			continue
		}
		s.m[saved.Hash] = &session{profile: saved.Profile, pid: saved.PID, start: saved.Start, expires: saved.Expires}
	}
	if pruned {
		_ = s.saveLocked()
	}
	return s
}

// saveLocked writes the current hashes atomically; the caller holds mu or is constructing the store.
func (s *sessionStore) saveLocked() error {
	state := sessionState{Schema: routerSessionsFormat.Version, Sessions: make([]persistedSession, 0, len(s.m))}
	for hash, ses := range s.m {
		state.Sessions = append(state.Sessions, persistedSession{
			Hash: hash, Profile: ses.profile, PID: ses.pid, Start: ses.start, Expires: ses.expires,
		})
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].Hash < state.Sessions[j].Hash })
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(s.file, raw, 0o600)
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
	pruned := false
	for k, v := range s.m {
		if !now.Before(v.expires) {
			delete(s.m, k)
			pruned = true
		}
	}
	if len(s.m) >= sessionMax { // exited-but-unexpired sessions must not block a new mint
		for k, v := range s.m {
			if !s.stillBound(v) {
				delete(s.m, k)
				pruned = true
			}
		}
	}
	if len(s.m) >= sessionMax {
		if pruned {
			_ = s.saveLocked()
		}
		return "", errors.New("too many live session tokens")
	}
	hash := sessionHash(tok)
	s.m[hash] = &session{profile: profile, pid: pid, start: start, expires: now.Add(sessionTTL), checked: now}
	if err := s.saveLocked(); err != nil {
		delete(s.m, hash)
		return "", fmt.Errorf("persist session: %w", err)
	}
	return tok, nil
}

func (s *sessionStore) revoke(tok string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sessionHash(tok))
	return s.saveLocked()
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
	hash := sessionHash(tok)
	for k, v := range s.m { // constant-time compare, no map lookup keyed on the presented bytes
		if subtle.ConstantTimeCompare([]byte(k), []byte(hash)) == 1 {
			ses = v
		}
	}
	if ses == nil {
		return false
	}
	recheck := now.Sub(ses.checked) >= sessionVerify
	if !now.Before(ses.expires) || (recheck && !s.stillBound(ses)) {
		delete(s.m, hash)
		_ = s.saveLocked()
		return false
	}
	if ses.profile != profile {
		return false
	}
	if recheck { // only a passed liveness check refreshes the trust window
		ses.checked = now
	}
	ses.expires = now.Add(sessionTTL)
	return true
}

// count reports live tokens, pruning expired tokens and tokens whose bound process has exited.
func (s *sessionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	pruned := false
	for hash, ses := range s.m {
		if !now.Before(ses.expires) || !s.stillBound(ses) {
			delete(s.m, hash)
			pruned = true
		}
	}
	if pruned {
		_ = s.saveLocked()
	}
	return len(s.m)
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
		if err := r.sessions.revoke(in.Token); err != nil {
			writeError(w, http.StatusInternalServerError, "proxy: persist session revocation: "+err.Error())
			return
		}
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

// RevokeSessionToken drops token at the daemon; if it is not running, no request can use the token.
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
