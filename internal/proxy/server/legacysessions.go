package server

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
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
	legacyPersist = time.Hour          // a slid expiry is written once it leads the saved one by this much
)

// legacySession is a token the previous build minted (cct_ + 64 hex), stored by hash; none is ever minted again.
type legacySession struct {
	Hash             string    `json:"hash"`
	Profiles         []string  `json:"profiles"`
	FailoverProvider string    `json:"failover_provider,omitempty"`
	PID              int       `json:"pid"`
	Start            string    `json:"start"`
	Expires          time.Time `json:"expires"`
	saved            time.Time // the expiry last written to disk
}

// legacyState is the previous build's sessions.json, kept in its format so that build still reads it.
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

// slideLegacyLocked renews an accepted old-format entry as the previous build did; a no-op for any other hash.
func (s *sessionStore) slideLegacyLocked(hash string, now time.Time) {
	e, ok := s.legacy[hash]
	if !ok {
		return
	}
	e.Expires = now.Add(legacyTTL)
	s.legacy[hash] = e
	if e.Expires.Sub(e.saved) >= legacyPersist {
		_ = s.saveLegacyLocked() // a failed write retries on the next accepted request
	}
}

// reloadLegacyLocked folds a changed sessions.json into memory, keeping any further slide this daemon made.
func (s *sessionStore) reloadLegacyLocked() {
	fi, err := os.Stat(s.legacyFile)
	if err != nil || sameVersion(s.legacyRead, fi) {
		return
	}
	if onDisk, err := readLegacy(s.legacyFile); err == nil {
		s.legacy = s.mergeLegacyLocked(onDisk, false)
		s.legacyRead = fi
	}
}

// mergeLegacyLocked is onDisk's live entries at the later of both expiries; drop also leaves out revoked and gone.
func (s *sessionStore) mergeLegacyLocked(onDisk []legacySession, drop bool) map[string]legacySession {
	now, out := s.now(), map[string]legacySession{}
	for _, e := range onDisk {
		if !validProfiles(e.Profiles) || e.PID <= 0 || e.Start == "" || (drop && s.droppedLegacyLocked(e.Hash)) {
			continue
		}
		e.saved = e.Expires
		if mem, ok := s.legacy[e.Hash]; ok && mem.Expires.After(e.Expires) {
			e.Expires = mem.Expires
		}
		if now.Before(e.Expires) {
			out[e.Hash] = e
		}
	}
	return out
}

func (s *sessionStore) droppedLegacyLocked(hash string) bool {
	_, revoked := s.revoked[hash]
	return revoked || s.legacyDead[hash]
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

// saveLegacyLocked rewrites sessions.json under its lock, slides kept; memory changes only once the write lands.
func (s *sessionStore) saveLegacyLocked() error {
	return paths.WithFileLock(s.legacyFile, func() error {
		onDisk, err := readLegacy(s.legacyFile)
		if err != nil { // never overwrite entries this daemon cannot read
			return fmt.Errorf("read %s: %w", s.legacyFile, err)
		}
		if len(onDisk) == 0 {
			return nil // nothing to bridge, and no reason to create the file
		}
		kept := s.mergeLegacyLocked(onDisk, true)
		st := legacyState{Schema: routerSessionsFormat.Version, Sessions: []legacySession{}}
		for _, e := range kept {
			st.Sessions = append(st.Sessions, e)
		}
		sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].Hash < st.Sessions[j].Hash })
		raw, err := json.Marshal(st)
		if err == nil {
			err = paths.AtomicWrite(s.legacyFile, raw, 0o600)
		}
		if err != nil {
			return err
		}
		for h, e := range kept {
			e.saved = e.Expires
			kept[h] = e
		}
		s.legacy, s.legacyDead = kept, map[string]bool{}
		if fi, err := os.Stat(s.legacyFile); err == nil {
			s.legacyRead = fi
		}
		return nil
	})
}

// pruneLegacy drops stored old-format entries whose process exited; probes run without mu.
func (s *sessionStore) pruneLegacy() {
	s.mu.Lock()
	entries := make([]legacySession, 0, len(s.legacy))
	for _, e := range s.legacy {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	dead := []string{}
	for _, e := range entries {
		if start, err := s.procStart(e.PID); errors.Is(err, proc.ErrNoProcess) || (err == nil && start != e.Start) {
			dead = append(dead, e.Hash)
		}
	}
	if len(dead) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range dead {
		s.legacyDead[h] = true
	}
	_ = s.saveLegacyLocked() // a failed write keeps legacyDead and retries on the next count
}

var routerSessionsFormat = schemas.Register(schemas.Format{
	Name: "router-sessions", Owner: "internal/proxy", Kind: schemas.Record, Version: 2,
	Glob:    proxy.SessionsFile,
	New:     func() any { return new(legacyState) },
	Keep:    "the previous build's session token hashes, bridged until each process exits; never written new",
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
