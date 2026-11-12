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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	sessionTTL     = 7 * 24 * time.Hour // idle lifetime, renewed per accepted request while the process lives
	sessionVerify  = 2 * time.Second    // how long a liveness check is trusted
	sessionPersist = time.Hour          // a slid expiry is written to disk once it leads the saved one by this much
	sessionMax     = 256
	sessionTokenPf = "cct_"
)

// session is one minted token: scoped to a sorted profile set, bound to a process identity.
type session struct {
	profiles []string
	pid      int
	start    string
	expires  time.Time
	saved    time.Time // the expiry last written to disk
	checked  time.Time
}

type persistedSession struct {
	Hash     string    `json:"hash"`
	Profiles []string  `json:"profiles"`
	PID      int       `json:"pid"`
	Start    string    `json:"start"`
	Expires  time.Time `json:"expires"`
}

type sessionState struct {
	Schema   int                `json:"schema"`
	Sessions []persistedSession `json:"sessions"`
}

func sessionsFile() string { return filepath.Join(paths.StateDir(), "proxy", "sessions.json") }

// sessionStore persists only token hashes, scoped to the config directory at creation.
type sessionStore struct {
	mu        sync.Mutex
	m         map[string]*session
	file      string
	now       func() time.Time
	procStart func(pid int) (string, error) // a seam for tests: the pid's start-time identity
	unsaved   bool                          // the last write failed: memory (e.g. a revocation) is ahead of disk
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
			profiles: saved.Profiles,
			pid:      saved.PID,
			start:    saved.Start,
			expires:  saved.Expires,
			saved:    saved.Expires,
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

// validProfiles reports whether profiles is a non-empty set of valid profile names.
func validProfiles(profiles []string) bool {
	return len(profiles) > 0 && !slices.ContainsFunc(profiles, func(p string) bool { return !validProfile(p) })
}

// validSavedSession reports whether a persisted record is well-formed and unexpired at now.
func validSavedSession(saved persistedSession, now time.Time) bool {
	hash, err := hex.DecodeString(saved.Hash)
	if err != nil || len(hash) != sha256.Size || saved.Hash != strings.ToLower(saved.Hash) {
		return false
	}
	return validProfiles(saved.Profiles) && saved.PID > 0 && saved.Start != "" && now.Before(saved.Expires)
}

// saveLocked writes the current hashes atomically; the caller holds mu or is constructing the store.
func (s *sessionStore) saveLocked() error {
	state := sessionState{Schema: routerSessionsFormat.Version, Sessions: make([]persistedSession, 0, len(s.m))}
	for hash, ses := range s.m {
		state.Sessions = append(state.Sessions, persistedSession{
			Hash: hash, Profiles: ses.profiles, PID: ses.pid, Start: ses.start, Expires: ses.expires,
		})
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].Hash < state.Sessions[j].Hash })
	raw, err := json.Marshal(state)
	if err == nil {
		err = paths.AtomicWrite(s.file, raw, 0o600)
	}
	s.unsaved = err != nil
	return err
}

// errPersist marks a mint that failed on the server's own disk write, not on the request.
var errPersist = errors.New("persist session")

// mint binds a new token for the profile set to pid's current identity; a missing process is an error.
func (s *sessionStore) mint(profiles []string, pid int) (string, error) {
	if !validProfiles(profiles) || pid <= 0 {
		return "", errors.New("profiles and pid are required")
	}
	profiles = slices.Compact(slices.Sorted(slices.Values(profiles)))
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
	hash := textutil.SHA256Hex([]byte(tok))
	s.m[hash] = &session{
		profiles: profiles,
		pid:      pid,
		start:    start,
		expires:  now.Add(sessionTTL),
		saved:    now.Add(sessionTTL),
		checked:  now,
	}
	if err := s.saveLocked(); err != nil {
		delete(s.m, hash)
		return "", fmt.Errorf("%w: %w", errPersist, err)
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

// revoke drops tok in memory even when the write fails (fail closed); count retries the write until it lands.
func (s *sessionStore) revoke(tok string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, textutil.SHA256Hex([]byte(tok)))
	return s.saveLocked()
}

// accept reports whether tok's set holds profile; success slides the TTL, a dead or reused process drops the token.
func (s *sessionStore) accept(tok, profile string) bool {
	if !strings.HasPrefix(tok, sessionTokenPf) || profile == "" {
		return false
	}
	now, hash := s.now(), textutil.SHA256Hex([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.findLocked(hash)
	if ses == nil {
		return false
	}
	recheck := now.Sub(ses.checked) >= sessionVerify
	if !s.liveLocked(hash, ses, now, recheck) || !slices.Contains(ses.profiles, profile) {
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

// profiles is tok's profile set, nil for an unknown token; it neither slides nor probes (accept already did).
func (s *sessionStore) profiles(tok string) []string {
	if !strings.HasPrefix(tok, sessionTokenPf) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ses := s.findLocked(textutil.SHA256Hex([]byte(tok))); ses != nil {
		return ses.profiles
	}
	return nil
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
	if s.dropLocked(dead) || pruned || s.unsaved {
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
	if errors.Is(err, proc.ErrNoProcess) {
		return false, true // an exited process is a verified miss, so probeUnlocked expires the token
	}
	if err != nil {
		return true, false // a failed probe keeps the token for pruning
	}
	return start == ses.start, true
}

// profileOfPath returns <name> of /p/<name>/..., "" for any other path.
func profileOfPath(path string) string {
	name, _, _ := parseRoute(path)
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
	tok, err := r.sessions.mint(in.Profiles, in.PID)
	if errors.Is(err, errPersist) {
		fmt.Fprintf(r.log, "proxy: session mint: %v\n", err)
		writeError(w, http.StatusInternalServerError, "proxy: session could not be persisted")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: session: "+err.Error())
		return
	}
	b, _ := json.Marshal(gateway.SessionResponse{Token: tok, ExpiresIn: int(sessionTTL.Seconds())})
	writeJSON(w, http.StatusOK, b)
}

var routerSessionsFormat = schemas.Register(schemas.Format{
	Name: "router-sessions", Owner: "internal/gateway", Kind: schemas.Record, Version: 2,
	Glob:    sessionsFile,
	New:     func() any { return new(sessionState) },
	Keep:    "process-bound session token hashes; retained across proxy restarts and pruned when expired or revoked",
	Migrate: migrateSessions,
})

// migrateSessions steps v0 -> v1 (stamp) and v1 -> v2 (each record's one "profile" becomes its "profiles" set).
func migrateSessions(from int, raw []byte) ([]byte, error) {
	if from != 1 {
		return schemas.SameShape(from, raw)
	}
	var v1 struct {
		Sessions []struct {
			persistedSession
			Profile string `json:"profile"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &v1); err != nil {
		return nil, err
	}
	next := sessionState{Schema: 2, Sessions: make([]persistedSession, 0, len(v1.Sessions))}
	for _, s := range v1.Sessions {
		s.Profiles = []string{s.Profile}
		next.Sessions = append(next.Sessions, s.persistedSession)
	}
	return json.Marshal(next)
}
