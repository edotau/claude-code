package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
		gone := false
		err := paths.WithFileLock(file, func() error { // the lock file stays: other processes may hold its inode
			cur, err := ReadScope(sc.Team, sc.ID) // a replacement may have published since the listing
			if err != nil || scopeLive(cur) {
				return err
			}
			gone = true
			return os.Remove(file)
		})
		if err == nil && gone {
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

// StopDetached stops every live detached scope of any id, re-scans once for late starts, and never touches an asker's.
func StopDetached() (stopped int, err error) {
	done := map[string]bool{}
	for range 2 {
		for _, sc := range DetachedScopes() {
			if k := sc.Team + "@" + sc.ID; !done[k] {
				done[k] = true
				if e := stopScope(sc); e != nil {
					err = errors.Join(err, e)
					continue
				}
				stopped++
			}
		}
	}
	return stopped, err
}

// drainIfIdle is the seam of retireSlot's drain request.
var drainIfIdle = DrainIfIdle

// DrainIfIdle: 200 idle (now draining), 409 busy; answered is false on an error, 401 or an older build's 404.
func DrainIfIdle(ctx context.Context, sc Scope) (idle, answered bool) {
	secret, err := ClientSecret()
	if err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Base(sc.Port)+DrainPath, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set(ClientHeader, secret)
	resp, err := sessionClient.Do(req)
	if err != nil {
		return false, false
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusConflict:
		return false, true
	}
	return false, false
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
