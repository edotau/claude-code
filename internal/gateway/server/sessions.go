package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	sessionTTL     = 12 * time.Hour  // idle lifetime, renewed per accepted request while the process lives
	sessionVerify  = 2 * time.Second // how long a liveness check is trusted
	sessionPersist = time.Hour       // a slid expiry is written to disk once it leads the saved one by this much
	sessionMax     = 256
	sessionTokenPf = "cct_"
)

// session is one minted token: scoped to a profile, bound to a process identity.
type session struct {
	profile string
	pid     int
	start   string
	expires time.Time
	saved   time.Time // the expiry last written to disk
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
		if !validSavedSession(saved, now()) {
			pruned = true
			continue
		}
		s.m[saved.Hash] = &session{
			profile: saved.Profile,
			pid:     saved.PID,
			start:   saved.Start,
			expires: saved.Expires,
			saved:   saved.Expires,
		}
	}
	if pruned {
		_ = s.saveLocked()
	}
	return s
}

// validProfile reports whether profile is non-empty and free of URL-structure characters.
func validProfile(profile string) bool {
	return profile != "" && !strings.ContainsAny(profile, "/?#")
}

// validSavedSession reports whether a persisted record is well-formed and unexpired at now.
func validSavedSession(saved persistedSession, now time.Time) bool {
	hash, err := hex.DecodeString(saved.Hash)
	if err != nil || len(hash) != sha256.Size || saved.Hash != strings.ToLower(saved.Hash) {
		return false
	}
	return validProfile(saved.Profile) && saved.PID > 0 && saved.Start != "" && now.Before(saved.Expires)
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
	if !validProfile(profile) || pid <= 0 {
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
	if err := s.makeRoomLocked(now); err != nil {
		return "", err
	}
	hash := sessionHash(tok)
	s.m[hash] = &session{
		profile: profile,
		pid:     pid,
		start:   start,
		expires: now.Add(sessionTTL),
		saved:   now.Add(sessionTTL),
		checked: now,
	}
	if err := s.saveLocked(); err != nil {
		delete(s.m, hash)
		return "", fmt.Errorf("persist session: %w", err)
	}
	return tok, nil
}

// makeRoomLocked drops expired and exited sessions so a mint fits; mu is held on entry and exit (released to probe).
func (s *sessionStore) makeRoomLocked(now time.Time) error {
	pruned := s.pruneExpiredLocked(now)
	var probe map[string]*session
	if len(s.m) >= sessionMax { // exited-but-unexpired sessions must not block a new mint
		probe = maps.Clone(s.m)
	}
	s.mu.Unlock()
	dead := s.unbound(probe)
	s.mu.Lock()
	pruned = s.dropLocked(dead) || pruned
	if len(s.m) >= sessionMax {
		if pruned {
			_ = s.saveLocked()
		}
		return errors.New("too many live session tokens")
	}
	return nil
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
	now, hash := s.now(), sessionHash(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.findLocked(hash)
	if ses == nil {
		return false
	}
	recheck := now.Sub(ses.checked) >= sessionVerify
	if !s.liveLocked(hash, ses, now, recheck) || ses.profile != profile {
		return false
	}
	if recheck { // only a passed liveness check refreshes the trust window
		ses.checked = now
	}
	s.slideLocked(ses, now)
	return true
}

// liveLocked reports whether ses is unexpired and, on recheck, still bound; an expired ses is dropped.
func (s *sessionStore) liveLocked(hash string, ses *session, now time.Time, recheck bool) bool {
	if recheck && now.Before(ses.expires) && !s.probeUnlocked(hash, ses) {
		return false
	}
	if !now.Before(ses.expires) {
		delete(s.m, hash)
		_ = s.saveLocked()
		return false
	}
	return true
}

// probeUnlocked probes ses without mu; false if ses was dropped or the probe failed, an exited process expires it.
func (s *sessionStore) probeUnlocked(hash string, ses *session) bool {
	s.mu.Unlock()
	bound, verified := s.identity(ses)
	s.mu.Lock()
	if s.findLocked(hash) != ses || !verified {
		return false
	}
	if !bound {
		ses.expires = time.Time{}
	}
	return true
}

// findLocked returns the session for hash by constant-time compare, no map lookup keyed on the presented bytes.
func (s *sessionStore) findLocked(hash string) *session {
	var ses *session
	for k, v := range s.m {
		if subtle.ConstantTimeCompare([]byte(k), []byte(hash)) == 1 {
			ses = v
		}
	}
	return ses
}

// slideLocked renews ses's TTL and persists the new expiry once it leads the saved one by sessionPersist.
func (s *sessionStore) slideLocked(ses *session, now time.Time) {
	ses.expires = now.Add(sessionTTL)
	if ses.expires.Sub(ses.saved) >= sessionPersist && s.saveLocked() == nil { // a failed write retries next request
		ses.saved = ses.expires
	}
}

// count reports live tokens, pruning expired tokens and tokens whose bound process has exited.
func (s *sessionStore) count() int {
	s.mu.Lock()
	pruned := s.pruneExpiredLocked(s.now())
	probe := maps.Clone(s.m)
	s.mu.Unlock()
	dead := s.unbound(probe) // procStart may exec ps (darwin): never under mu
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropLocked(dead) || pruned {
		_ = s.saveLocked()
	}
	return len(s.m)
}

// pruneExpiredLocked drops expired sessions and reports whether any went.
func (s *sessionStore) pruneExpiredLocked(now time.Time) bool {
	pruned := false
	for k, v := range s.m {
		if !now.Before(v.expires) {
			delete(s.m, k)
			pruned = true
		}
	}
	return pruned
}

// unbound probes each session's process without mu and returns those whose process exited or was reused.
func (s *sessionStore) unbound(probe map[string]*session) map[string]*session {
	dead := map[string]*session{}
	for hash, ses := range probe {
		if bound, _ := s.identity(ses); !bound {
			dead[hash] = ses
		}
	}
	return dead
}

// dropLocked deletes each dead session still mapped to the same pointer and reports whether any went.
func (s *sessionStore) dropLocked(dead map[string]*session) bool {
	dropped := false
	for hash, ses := range dead {
		if s.m[hash] == ses {
			delete(s.m, hash)
			dropped = true
		}
	}
	return dropped
}

// identity probes ses's pid; verified is false when the probe itself failed, so bound is only a guess then.
func (s *sessionStore) identity(ses *session) (bound, verified bool) {
	start, err := s.procStart(ses.pid)
	if err != nil {
		return !errors.Is(err, proc.ErrNoProcess), false // a failed probe keeps the token for pruning
	}
	return start == ses.start, true
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

// serveSession handles the mint and revoke endpoints, after the caller proved the router secret.
func (r *Router) serveSession(w http.ResponseWriter, req *http.Request) {
	var in gateway.SessionRequest
	if req.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "proxy: want POST with a JSON body")
		return
	}
	if req.URL.Path == gateway.SessionRevokePath {
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
	writeJSON(w, http.StatusOK, mustJSON(gateway.SessionResponse{Token: tok, ExpiresIn: int(sessionTTL.Seconds())}))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var routerSessionsFormat = schemas.Register(schemas.Format{
	Name: "router-sessions", Owner: "internal/gateway", Kind: schemas.Record, Version: 1,
	Glob:    sessionsFile,
	New:     func() any { return new(sessionState) },
	Keep:    "process-bound session token hashes; retained across proxy restarts and pruned when expired or revoked",
	Migrate: schemas.SameShape,
})
