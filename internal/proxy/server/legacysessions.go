package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

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
	s.reloadLocked()
	e, ok := s.legacy[hash]
	if _, revoked := s.revoked[hash]; !ok || revoked || s.legacyDead[hash] || !s.now().Before(e.Expires) {
		return proxy.SessionClaims{}, "", false
	}
	return proxy.SessionClaims{Profiles: e.Profiles, Failover: e.FailoverProvider, PID: e.PID, Start: e.Start}, hash, true
}

// slideLegacyLocked renews an accepted old-format entry in memory as the previous build did; a no-op for other hashes.
func (s *sessionStore) slideLegacyLocked(hash string, now time.Time) {
	if e, ok := s.legacy[hash]; ok {
		e.Expires = now.Add(legacyTTL)
		s.legacy[hash] = e
	}
}

// reloadLegacyLocked folds a changed sessions.json into memory, keeping any further slide this daemon made.
func (s *sessionStore) reloadLegacyLocked() {
	fi, err := os.Stat(s.legacyFile)
	if err != nil || sameVersion(s.legacyRead, fi) {
		return
	}
	if onDisk, err := readLegacy(s.legacyFile); err == nil {
		s.legacy, s.legacyRead = s.liveLegacyLocked(onDisk), fi
	}
}

// liveLegacyLocked is onDisk's well-formed, unexpired entries, each at the later of its saved and in-memory expiry.
func (s *sessionStore) liveLegacyLocked(onDisk []legacySession) map[string]legacySession {
	now, out := s.now(), map[string]legacySession{}
	for _, e := range onDisk {
		if !validProfiles(e.Profiles) || e.PID <= 0 || e.Start == "" {
			continue
		}
		if mem, ok := s.legacy[e.Hash]; ok && mem.Expires.After(e.Expires) {
			e.Expires = mem.Expires
		}
		if now.Before(e.Expires) {
			out[e.Hash] = e
		}
	}
	return out
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
