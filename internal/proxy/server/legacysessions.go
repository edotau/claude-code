package server

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/textutil"
)

// Bridge, removal condition: delete this file once sessions.json holds no "sessions" entry, or at the next release.

// legacySession is a token the previous build minted (cct_ + 64 hex), stored by hash; none is ever minted again.
type legacySession struct {
	Hash             string    `json:"hash"`
	Profiles         []string  `json:"profiles"`
	FailoverProvider string    `json:"failover_provider,omitempty"`
	PID              int       `json:"pid"`
	Start            string    `json:"start"`
	Expires          time.Time `json:"expires"`
}

// legacyClaims is a stored old-format tok's binding; its expiry is unchecked (the old build slid it), the pid limits.
func (s *sessionStore) legacyClaims(tok string) (proxy.SessionClaims, bool) {
	h, ok := strings.CutPrefix(tok, proxy.SessionTokenPrefix) // the old shape: cct_ + 64 lowercase hex
	if _, err := hex.DecodeString(h); !ok || len(h) != 64 || h != strings.ToLower(h) || err != nil {
		return proxy.SessionClaims{}, false
	}
	hash := textutil.SHA256Hex([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	e, ok := s.legacy[hash]
	if _, revoked := s.revoked[hash]; !ok || revoked || s.legacyDead[hash] {
		return proxy.SessionClaims{}, false
	}
	return proxy.SessionClaims{Profiles: e.Profiles, Failover: e.FailoverProvider, PID: e.PID, Start: e.Start}, true
}

// keptLegacyLocked is onDisk's entries minus the revoked and the seen-gone; memory takes the same set.
func (s *sessionStore) keptLegacyLocked(onDisk []legacySession) []legacySession {
	out := []legacySession{}
	for _, e := range onDisk {
		if _, revoked := s.revoked[e.Hash]; !revoked && !s.legacyDead[e.Hash] {
			out = append(out, e)
		}
	}
	clear(s.legacyDead) // every dead hash is now off disk; the old build never re-adds an exited process
	s.setLegacyLocked(out)
	return out
}

// setLegacyLocked replaces memory's legacy entries with the well-formed ones of entries.
func (s *sessionStore) setLegacyLocked(entries []legacySession) {
	s.legacy = map[string]legacySession{}
	for _, e := range entries {
		if validProfiles(e.Profiles) && e.PID > 0 && e.Start != "" {
			s.legacy[e.Hash] = e
		}
	}
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
	_ = s.saveLocked() // a failed write retries on the next count
}
