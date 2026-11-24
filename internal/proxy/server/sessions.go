package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	sessionVerify = 2 * time.Second // how long a liveness check is trusted
	sessionMax    = 256             // bound on the in-memory seen set
)

// seenSession is a token this daemon accepted: a liveness cache and the live count, memory only.
type seenSession struct {
	pid     int
	start   string
	checked time.Time
}

type revokedSession struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

type revokedState struct {
	Schema  int              `json:"schema"`
	Revoked []revokedSession `json:"revoked"`
}

// sessionStore verifies signed tokens; it persists only revocations (merged per write) and the legacy bridge.
type sessionStore struct {
	mu         sync.Mutex
	secret     string
	seen       map[string]*seenSession  // token hash -> last verified binding
	revoked    map[string]time.Time     // token hash -> the token's expiry
	unsaved    bool                     // the last revocation write failed: count retries it
	read       os.FileInfo              // the revocation file version last folded in
	file       string                   // revoked.json
	legacy     map[string]legacySession // token hash -> the previous build's stored binding
	legacyDead map[string]bool          // legacy hashes whose process was seen gone, dropped on the next write
	legacyRead os.FileInfo              // the sessions.json version last folded in
	legacyFile string                   // sessions.json, the previous build's file
	now        func() time.Time
	procStart  func(pid int) (string, error) // a seam for tests: the pid's start-time identity
}

func newSessionStore(secret string, now func() time.Time) *sessionStore {
	s := &sessionStore{secret: secret, seen: map[string]*seenSession{}, revoked: map[string]time.Time{},
		legacy: map[string]legacySession{}, legacyDead: map[string]bool{}, legacyFile: proxy.SessionsFile(),
		file: proxy.RevokedFile(), now: now, procStart: proc.StartTime}
	s.mu.Lock()
	s.reloadLocked()
	s.mu.Unlock()
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

// sameVersion reports fi is the file version read was taken from (an atomic write is a new inode).
func sameVersion(read, fi os.FileInfo) bool {
	return read != nil && os.SameFile(read, fi) && fi.ModTime().Equal(read.ModTime()) && fi.Size() == read.Size()
}

// reloadLocked folds both files into memory when their version changed and ages out expired revocations.
func (s *sessionStore) reloadLocked() {
	now := s.now()
	maps.DeleteFunc(s.revoked, func(_ string, exp time.Time) bool { return !now.Before(exp) })
	s.reloadLegacyLocked()
	fi, err := os.Stat(s.file)
	if err != nil || sameVersion(s.read, fi) {
		return
	}
	if got, err := readRevoked(s.file, now); err == nil { // a failed read retries on the next request
		maps.Copy(s.revoked, got)
		s.read = fi
	}
}

// readRevoked is the file's unexpired revocations: none when absent, an error when present but unreadable.
func readRevoked(file string, now time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	var state revokedState
	if err == nil {
		_, err = schemas.Decode(routerRevokedFormat, raw, &state)
	}
	if err != nil {
		return nil, err
	}
	for _, r := range state.Revoked {
		if now.Before(r.Expires) {
			out[r.Hash] = r.Expires
		}
	}
	return out, nil
}

// saveLocked merges memory with the file under its lock, so two daemons on one config never lose a revocation.
func (s *sessionStore) saveLocked() error {
	err := paths.WithFileLock(s.file, func() error {
		now := s.now()
		onDisk, err := readRevoked(s.file, now)
		if err != nil { // never overwrite a list this daemon cannot read: it may hold another daemon's revocations
			return fmt.Errorf("read %s: %w", s.file, err)
		}
		maps.Copy(s.revoked, onDisk)
		state := revokedState{Schema: routerRevokedFormat.Version, Revoked: []revokedSession{}}
		for h, exp := range s.revoked {
			if now.Before(exp) {
				state.Revoked = append(state.Revoked, revokedSession{Hash: h, Expires: exp})
			} else {
				delete(s.revoked, h)
			}
		}
		sort.Slice(state.Revoked, func(i, j int) bool { return state.Revoked[i].Hash < state.Revoked[j].Hash })
		raw, err := json.Marshal(state)
		if err == nil {
			err = paths.AtomicWrite(s.file, raw, 0o600)
		}
		if fi, serr := os.Stat(s.file); err == nil && serr == nil {
			s.read = fi
		}
		return err
	})
	s.unsaved = err != nil
	return err
}

// mintWithFailover signs a token for profiles bound to pid's identity, with an optional same-provider failover group.
func (s *sessionStore) mintWithFailover(profiles []string, pid int, failoverProvider string) (string, error) {
	if !validProfiles(profiles) || pid <= 0 || (failoverProvider != "" && !validProfile(failoverProvider)) {
		return "", errors.New("profiles and pid are required")
	}
	start, err := s.procStart(pid)
	if err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	c := proxy.SessionClaims{Profiles: profiles, Failover: failoverProvider, PID: pid, Start: start}
	return proxy.SignSessionToken(s.secret, c, s.now())
}

// mint is mintWithFailover with no failover group.
func (s *sessionStore) mint(profiles []string, pid int) (string, error) {
	return s.mintWithFailover(profiles, pid, "")
}

// revoke denies tok on every daemon of this config; an unsigned token needs no entry.
func (s *sessionStore) revoke(tok string) error {
	var expires time.Time
	_, _, legacy := s.legacyClaims(tok)
	if c, err := proxy.ParseSessionToken(s.secret, tok); err == nil {
		expires = time.Unix(c.Expires, 0)
	} else if legacy {
		expires = s.now().Add(legacyTTL + legacyPersist) // outlives the entry's latest possible saved expiry
	} else {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := textutil.SHA256Hex([]byte(tok))
	s.revoked[hash] = expires
	delete(s.seen, hash)
	if legacy {
		_ = s.saveLegacyLocked() // the revocation already refuses it; this only tidies sessions.json
	}
	return s.saveLocked()
}

// accept reports whether tok is a live, unrevoked token whose set holds profile.
func (s *sessionStore) accept(tok, profile string) bool {
	if !strings.HasPrefix(tok, proxy.SessionTokenPrefix) || profile == "" {
		return false
	}
	return s.check(tok, profile) == ""
}

const (
	whyExpired = "session token expired"
	whyExited  = "the process the session token was bound to has exited"
	whyRevoked = "session token was revoked"
	whyUnknown = "the session token's process could not be verified"
)

// check is "" when tok serves profile now, else why not; a passed liveness probe is trusted for sessionVerify.
func (s *sessionStore) check(tok, profile string) string {
	c, why := s.verify(tok)
	if why != "" {
		return why
	}
	now, hash := s.now(), textutil.SHA256Hex([]byte(tok))
	if !slices.Contains(c.Profiles, profile) {
		return "session token is not scoped to this route (" + strings.Join(c.Profiles, ",") + " only)"
	}
	s.mu.Lock()
	revoked, fresh := s.stateLocked(hash, now)
	s.mu.Unlock()
	if revoked {
		return whyRevoked
	}
	if !fresh {
		if why := s.probe(c); why != "" {
			return why
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, revoked := s.revoked[hash]; revoked { // a revoke that landed during the probe still wins
		return whyRevoked
	}
	if !fresh { // a fresh entry keeps its checked time: frequent use never extends the trust window
		s.noteSeenLocked(hash, c, now)
	}
	s.slideLegacyLocked(hash, now)
	return ""
}

// verify is tok's claims when signed and inside its window, or a stored legacy binding; else why not.
func (s *sessionStore) verify(tok string) (proxy.SessionClaims, string) {
	if c, _, ok := s.legacyClaims(tok); ok {
		return c, ""
	}
	c, err := proxy.ParseSessionToken(s.secret, tok)
	if err != nil {
		return c, err.Error()
	}
	if proxy.SessionWindowErr(c, s.now()) != nil {
		return c, whyExpired
	}
	return c, ""
}

// stateLocked reloads revocations and reports whether hash is revoked and its liveness check still trusted.
func (s *sessionStore) stateLocked(hash string, now time.Time) (revoked, fresh bool) {
	s.reloadLocked()
	_, revoked = s.revoked[hash]
	ent := s.seen[hash]
	return revoked, ent != nil && now.Sub(ent.checked) < sessionVerify
}

// probe checks c's pid still names the process it was minted for; it never holds mu (ps may exec on darwin).
func (s *sessionStore) probe(c proxy.SessionClaims) string {
	start, err := s.procStart(c.PID)
	switch {
	case errors.Is(err, proc.ErrNoProcess):
		return whyExited
	case err != nil:
		return whyUnknown
	case start != c.Start:
		return whyExited
	}
	return ""
}

// noteSeenLocked records a verified binding, evicting the oldest check at the cap.
func (s *sessionStore) noteSeenLocked(hash string, c proxy.SessionClaims, now time.Time) {
	if _, ok := s.seen[hash]; !ok && len(s.seen) >= sessionMax {
		oldest := ""
		for k, v := range s.seen {
			if oldest == "" || v.checked.Before(s.seen[oldest].checked) {
				oldest = k
			}
		}
		delete(s.seen, oldest)
	}
	s.seen[hash] = &seenSession{pid: c.PID, start: c.Start, checked: now}
}

// refusal names why tok was refused on profile's route; it never returns any part of tok.
func (s *sessionStore) refusal(tok, profile string) string {
	if tok == "" {
		return "no token header sent"
	}
	if why := s.check(tok, profile); why != "" {
		return why
	}
	return whyUnknown
}

// claims is tok's verified claims, ok false for anything unsigned; accept already checked liveness.
func (s *sessionStore) claims(tok string) (proxy.SessionClaims, bool) {
	if !strings.HasPrefix(tok, proxy.SessionTokenPrefix) {
		return proxy.SessionClaims{}, false
	}
	if c, _, ok := s.legacyClaims(tok); ok {
		return c, true
	}
	c, err := proxy.ParseSessionToken(s.secret, tok)
	return c, err == nil
}

// profiles is tok's profile set, nil for an unsigned token.
func (s *sessionStore) profiles(tok string) []string {
	c, _ := s.claims(tok)
	return c.Profiles
}

// failoverProvider is tok's server-side failover provider group, or "" when pinned to profiles only.
func (s *sessionStore) failoverProvider(tok string) string {
	c, _ := s.claims(tok)
	return c.Failover
}

// count is the tokens this daemon accepted whose process still lives; it is shown, never consulted for auth.
func (s *sessionStore) count() int {
	s.pruneLegacy()
	s.mu.Lock()
	if s.unsaved { // a revocation still only in memory: retry its write
		_ = s.saveLocked()
	}
	probe := maps.Clone(s.seen)
	s.mu.Unlock()
	dead := []string{}
	for hash, ses := range probe {
		if start, err := s.procStart(ses.pid); errors.Is(err, proc.ErrNoProcess) || (err == nil && start != ses.start) {
			dead = append(dead, hash)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range dead {
		delete(s.seen, h)
	}
	return len(s.seen)
}

// profileOfPath returns <name> of /p/<name>/..., "" for any other path.
func profileOfPath(path string) string {
	name, _, _ := parseRoute(path)
	return name
}

// serveSession handles the mint and revoke endpoints, after the caller proved the router secret.
func (r *Router) serveSession(w http.ResponseWriter, req *http.Request) {
	var in proxy.SessionRequest
	if req.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "proxy: want POST with a JSON body")
		return
	}
	if req.URL.Path == proxy.SessionRevokePath {
		if err := r.sessions.revoke(in.Token); err != nil {
			writeError(w, http.StatusInternalServerError, "proxy: persist session revocation: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tok, err := r.sessions.mintWithFailover(in.Profiles, in.PID, in.FailoverProvider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: session: "+err.Error())
		return
	}
	b, _ := json.Marshal(proxy.SessionResponse{Token: tok, ExpiresIn: int(proxy.SessionTokenTTL.Seconds())})
	writeJSON(w, http.StatusOK, b)
}

var routerRevokedFormat = schemas.Register(schemas.Format{
	Name: "router-revoked", Owner: "internal/proxy", Kind: schemas.Record, Version: 1,
	Glob:    proxy.RevokedFile,
	New:     func() any { return new(revokedState) },
	Keep:    "revoked session token hashes until each token's expiry; tokens themselves are signed, never stored",
	Migrate: schemas.SameShape,
})
