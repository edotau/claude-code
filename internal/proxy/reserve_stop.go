package proxy

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// SweepScopes removes the record and lock of every scope whose process is gone; it returns the files it removed.
func SweepScopes() []string {
	var removed []string
	scopes, _ := ListScopes()
	for _, sc := range scopes {
		if scopeLive(sc) {
			continue
		}
		file := scopeFile(sc.Team, sc.ID)
		err := paths.WithFileLock(file, func() error {
			cur, err := ReadScope(sc.Team, sc.ID) // a replacement may have published since the listing
			if err != nil || scopeLive(cur) {
				return err
			}
			return os.Remove(file)
		})
		if err == nil {
			_ = os.Remove(file + ".lock")
			removed = append(removed, file)
		}
	}
	return removed
}

// DetachedScopes returns the live standing proxies, after sweeping the dead records.
func DetachedScopes() []Scope {
	SweepScopes()
	scopes, _ := ListScopes()
	var out []Scope
	for _, sc := range scopes {
		if sc.Host == HostDetached && scopeLive(sc) {
			out = append(out, sc)
		}
	}
	return out
}

// StopDetached SIGTERMs every live detached scope and waits for each to exit; asker-hosted scopes are never touched.
func StopDetached() (stopped int, err error) {
	for _, sc := range DetachedScopes() {
		if e := stopScope(sc); e != nil {
			err = errors.Join(err, e)
			continue
		}
		stopped++
	}
	return stopped, err
}

// ScopeLine describes one live detached scope for status and doctor.
func ScopeLine(sc Scope, inflight int64) string {
	state := "current"
	if ScopeStale(sc) {
		state = "stale"
	}
	return fmt.Sprintf("scope %s@%s pid=%d port=%d build=%s %s inflight=%d up=%s", sc.Team, sc.ID, sc.PID, sc.Port,
		sc.Build, state, inflight, time.Since(sc.Started).Round(time.Second))
}
