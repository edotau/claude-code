package router

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// State stamp checks are shared only across a short burst; daemons recheck external writers every 2 ms.
const stateRecheckWindow = 2 * time.Millisecond

var stateMemo struct {
	sync.Mutex
	path  string
	at    time.Time
	state State
	key   [32]byte
}

func memoState() State {
	s, _, _ := readStateMemo()
	return s
}

func readStateMemo() (State, [32]byte, error) {
	path := StatePath()
	stateMemo.Lock()
	defer stateMemo.Unlock()
	if stateMemo.path == path && time.Since(stateMemo.at) < stateRecheckWindow {
		return stateMemo.state, stateMemo.key, nil
	}
	at := time.Now()
	s, err := stateCache.Get(path, loadState)
	if err != nil {
		return s, [32]byte{}, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return s, [32]byte{}, err
	}
	key := sha256.Sum256(data)
	stateMemo.path, stateMemo.at, stateMemo.state, stateMemo.key = path, at, s, key
	return s, key, nil
}

func invalidateStateMemo() {
	stateMemo.Lock()
	defer stateMemo.Unlock()
	stateMemo.path, stateMemo.state = "", nil
}

// RoutingStateKey binds routing reuse to quota content and endpoint-mark expiry without exposing mutable state.
func RoutingStateKey(now time.Time) ([32]byte, error) {
	s, key, err := readStateMemo()
	if err != nil {
		return [32]byte{}, err
	}
	data, err := json.Marshal([]any{key, s.routingHolds(now)})
	return sha256.Sum256(data), err
}

// Generation stamps the on-disk eligibility inputs at one moment; Holds reports that none has changed since.
type Generation struct {
	files []os.FileInfo // per generationInputs entry; nil when absent
	until time.Time     // the earliest quota-state boundary, capped at RacyWindow for listing and login TTLs
	ok    bool
}

// generationInputs hold quota, auth, cooling and model marks, dead logins, token expiry, listings and credentials.
func generationInputs() []string {
	state := paths.StateDir()
	return []string{StatePath(), filepath.Join(state, "cache", "login-state"), filepath.Join(state, "cache", "token-expiry"),
		filepath.Join(state, "preflight"), vault.Path()}
}

// TakeGeneration precedes a pick; a racy input never holds, except quota.json, whose every write is a rename.
func TakeGeneration(now time.Time) Generation {
	g := Generation{until: now.Add(paths.RacyWindow), ok: true}
	for i, path := range generationInputs() {
		fi, err := os.Stat(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) || i > 0 && fi != nil && fi.ModTime().After(now.Add(-paths.RacyWindow)) {
			return Generation{}
		}
		g.files = append(g.files, fi)
	}
	if t := sharedState().nextBoundary(now); !t.IsZero() && t.Before(g.until) {
		g.until = t
	}
	return g
}

// Holds is true while every input keeps its stamp and no quota mark, TTL or window has crossed a boundary.
func (g Generation) Holds(now time.Time) bool {
	if !g.ok || !now.Before(g.until) {
		return false
	}
	for i, path := range generationInputs() {
		fi, err := os.Stat(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) || !sameStamp(g.files[i], fi) {
			return false
		}
	}
	return true
}

func sameStamp(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// nextBoundary is the earliest instant after now at which a pick over s could read differently.
func (s State) nextBoundary(now time.Time) time.Time {
	var next time.Time
	at := func(t time.Time, ttl time.Duration) {
		if t.IsZero() {
			return
		}
		if t = t.Add(ttl); t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, e := range s {
		at(e.ExhaustedUntil, 0)
		at(e.ResetsAt, 0)
		at(e.Seen, StaleAfter)
		at(e.Checked, StaleAfter)
		at(e.AuthAt, authMarkTTL)
		for _, holds := range []struct {
			m   map[string]time.Time
			ttl time.Duration
		}{{e.Cooling, 0}, {e.ModelDead, 0}, {e.Unserved, unservedTTL}, {e.DiscoverySkip, DiscoverySkipTTL}} {
			for _, t := range holds.m {
				at(t, holds.ttl)
			}
		}
		for _, v := range e.Verified {
			at(v.ValidUntil, 0)
		}
	}
	return next
}

// routingHolds retains time boundaries in keys even when the state file itself has not changed.
func (s State) routingHolds(now time.Time) map[string][]string {
	out := map[string][]string{}
	for account, e := range s {
		for model, at := range e.Unserved {
			if now.Sub(at) < unservedTTL {
				out[account] = append(out[account], "unserved|"+model)
			}
		}
		for model, until := range e.ModelDead {
			if until.After(now) {
				out[account] = append(out[account], "quota|"+model)
			}
		}
		for model, at := range e.DiscoverySkip {
			if now.Sub(at) < DiscoverySkipTTL {
				out[account] = append(out[account], "skip|"+model)
			}
		}
	}
	for _, ids := range out {
		slices.Sort(ids)
	}
	return out
}
