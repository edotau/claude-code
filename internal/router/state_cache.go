package router

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sync"
	"time"
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
