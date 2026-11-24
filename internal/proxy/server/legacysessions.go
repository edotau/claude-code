package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// Bridge, removal condition: delete this file once sessions.json holds no "sessions" entry, or at the next release.

const (
	legacyTTL     = 7 * 24 * time.Hour // the previous build's idle lifetime, renewed per accepted request
	legacyPersist = time.Hour          // the previous build wrote a slid expiry once it led the saved one by this much
)

// legacySession is a token the previous build minted (cct_ + 64 hex), stored by hash; none is ever minted again.
type legacySession struct {
	Hash             string    `json:"hash"`
	Profiles         []string  `json:"profiles"`
	FailoverProvider string    `json:"failover_provider,omitempty"`
	PID              int       `json:"pid"`
	Start            string    `json:"start"`
	Expires          time.Time `json:"expires"`
}

// legacyState is the previous build's sessions.json, read only: that build may still write it without a lock.
type legacyState struct {
	Schema   int             `json:"schema"`
	Sessions []legacySession `json:"sessions"`
}

// legacyClaims is a stored, live-window old-format tok's binding and hash; ok is false for anything else.
func (s *sessionStore) legacyClaims(tok string) (proxy.SessionClaims, string, bool) {
	h, ok := strings.CutPrefix(tok, proxy.SessionTokenPrefix) // the old shape: cct_ + 64 lowercase hex
	if _, err := hex.DecodeString(h); !ok || len(h) != 64 || h != strings.ToLower(h) || err != nil {
		return proxy.SessionClaims{}, "", false
	}
	hash := textutil.SHA256Hex([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLegacyLocked()
	e, ok := s.legacy[hash]
	if !ok || s.legacyDead[hash] || !s.now().Before(e.Expires) {
		return proxy.SessionClaims{}, "", false
	}
	return proxy.SessionClaims{Profiles: e.Profiles, Failover: e.FailoverProvider, PID: e.PID, Start: e.Start}, hash, true
}

// legacyRenewal is a slid expiry of an old-format token, bound to the holder its sessions.json entry names.
type legacyRenewal struct {
	Hash    string    `json:"hash"`
	PID     int       `json:"pid"`
	Start   string    `json:"start"`
	Expires time.Time `json:"expires"`
}

// renewalState is legacy-renewals.json: written by this build (merged under its lock), never by the previous one.
type renewalState struct {
	Schema   int             `json:"schema"`
	Renewals []legacyRenewal `json:"renewals"`
}

// slideLegacyLocked renews an accepted old-format entry as the previous build did, persisting it hourly.
func (s *sessionStore) slideLegacyLocked(hash string, now time.Time) {
	e, ok := s.legacy[hash]
	if !ok {
		return
	}
	e.Expires = now.Add(legacyTTL)
	s.legacy[hash] = e
	if r, ok := s.renewed[hash]; ok && e.Expires.Sub(r.Expires) < legacyPersist {
		return
	}
	_ = s.saveRenewalsLocked() // a failed write keeps the slide in memory; the next accepted use retries it
}

// reloadLegacyLocked folds a changed sessions.json or renewal file into memory, keeping this daemon's own slides.
func (s *sessionStore) reloadLegacyLocked() {
	fi, err := os.Stat(s.legacyFile)
	if err != nil {
		return
	}
	rfi, rerr := os.Stat(s.renewFile)
	if sameVersion(s.legacyRead, fi) && (rerr != nil || sameVersion(s.renewRead, rfi)) {
		return
	}
	onDisk, err := readLegacy(s.legacyFile)
	if err != nil {
		return
	}
	if got, err := readRenewals(s.renewFile); err == nil && rerr == nil { // unreadable: saved expiries only
		s.renewed, s.renewRead = got, rfi
	}
	s.legacy, s.legacyRead = s.liveLegacyLocked(onDisk), fi
}

// liveLegacyLocked is onDisk's well-formed, unexpired entries, each at its latest saved, renewed or memory expiry.
func (s *sessionStore) liveLegacyLocked(onDisk []legacySession) map[string]legacySession {
	now, out := s.now(), map[string]legacySession{}
	for _, e := range onDisk {
		if !validProfiles(e.Profiles) || e.PID <= 0 || e.Start == "" {
			continue
		}
		e.Expires = s.renewedExpiryLocked(e, now)
		if mem, ok := s.legacy[e.Hash]; ok && mem.Expires.After(e.Expires) {
			e.Expires = mem.Expires
		}
		if now.Before(e.Expires) {
			out[e.Hash] = e
		}
	}
	return out
}

// renewedExpiryLocked is e's persisted renewal when it names e's holder and lies within one idle period, else e's own.
func (s *sessionStore) renewedExpiryLocked(e legacySession, now time.Time) time.Time {
	r, ok := s.renewed[e.Hash]
	if !ok || r.PID != e.PID || r.Start != e.Start || !r.Expires.After(e.Expires) || r.Expires.After(now.Add(legacyTTL)) {
		return e.Expires
	}
	return r.Expires
}

// readRenewals is the renewal file's records: none when absent, an error when present but unreadable.
func readRenewals(file string) (map[string]legacyRenewal, error) {
	out := map[string]legacyRenewal{}
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	var st renewalState
	if err == nil {
		_, err = schemas.Decode(routerRenewalsFormat, raw, &st)
	}
	if err != nil {
		return nil, err
	}
	for _, r := range st.Renewals {
		out[r.Hash] = r
	}
	return out, nil
}

// saveRenewalsLocked merges this daemon's slides into the file under its lock, dropping expired and dead holders.
func (s *sessionStore) saveRenewalsLocked() error {
	return paths.WithFileLock(s.renewFile, func() error {
		merged, err := readRenewals(s.renewFile)
		if err != nil { // never overwrite renewals this daemon cannot read: another daemon may hold them
			return err
		}
		for h, e := range s.legacy {
			if r, ok := merged[h]; !ok || e.Expires.After(r.Expires) {
				merged[h] = legacyRenewal{Hash: h, PID: e.PID, Start: e.Start, Expires: e.Expires}
			}
		}
		now := s.now()
		maps.DeleteFunc(merged, func(h string, r legacyRenewal) bool { return !now.Before(r.Expires) || s.legacyDead[h] })
		st := renewalState{Schema: routerRenewalsFormat.Version, Renewals: slices.SortedFunc(maps.Values(merged),
			func(a, b legacyRenewal) int { return strings.Compare(a.Hash, b.Hash) })}
		raw, err := json.Marshal(st)
		if err == nil {
			err = paths.AtomicWrite(s.renewFile, raw, 0o600)
		}
		if err == nil {
			s.renewed = merged
			s.renewRead, _ = os.Stat(s.renewFile)
		}
		return err
	})
}

// readLegacy is sessions.json's entries: none when absent, an error when present but unreadable.
func readLegacy(file string) ([]legacySession, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var st legacyState
	if err == nil {
		_, err = schemas.Decode(routerSessionsFormat, raw, &st)
	}
	return st.Sessions, err
}

// pruneLegacy marks old-format entries whose process exited; probes run without mu.
func (s *sessionStore) pruneLegacy() {
	s.mu.Lock()
	entries := make([]legacySession, 0, len(s.legacy))
	for _, e := range s.legacy {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		if start, err := s.procStart(e.PID); errors.Is(err, proc.ErrNoProcess) || (err == nil && start != e.Start) {
			s.mu.Lock()
			s.legacyDead[e.Hash] = true
			if _, ok := s.renewed[e.Hash]; ok { // a dead holder's renewal leaves the file
				_ = s.saveRenewalsLocked()
			}
			s.mu.Unlock()
		}
	}
}

var routerSessionsFormat = schemas.Register(schemas.Format{
	Name: "router-sessions", Owner: "internal/proxy", Kind: schemas.Record, Version: 2,
	Glob:    proxy.SessionsFile,
	New:     func() any { return new(legacyState) },
	Keep:    "the previous build's session token hashes, bridged until each process exits; read only, never written",
	Migrate: migrateSessions,
})

var routerRenewalsFormat = schemas.Register(schemas.Format{
	Name: "router-legacy-renewals", Owner: "internal/proxy", Kind: schemas.Record, Version: 1,
	Glob:    proxy.LegacyRenewalsFile,
	New:     func() any { return new(renewalState) },
	Keep:    "renewed expiries of the previous build's tokens by hash, until each expires or its process exits",
	Migrate: schemas.SameShape,
})

// migrateSessions steps v0 -> v1 (stamp) and v1 -> v2 (each record's one "profile" becomes its "profiles" set).
func migrateSessions(from int, raw []byte) ([]byte, error) {
	if from != 1 {
		return schemas.SameShape(from, raw)
	}
	var v1 struct {
		Sessions []struct {
			legacySession
			Profile string `json:"profile"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &v1); err != nil {
		return nil, err
	}
	next := legacyState{Schema: 2, Sessions: make([]legacySession, 0, len(v1.Sessions))}
	for _, s := range v1.Sessions {
		s.Profiles = []string{s.Profile}
		next.Sessions = append(next.Sessions, s.legacySession)
	}
	return json.Marshal(next)
}
