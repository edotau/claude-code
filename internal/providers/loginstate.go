package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// LoginState is a profile's dead user login: State is "needs_login" or "rejected", Since when it was first seen dead.
type LoginState struct {
	Profile, State string
	Since          time.Time
}

// Command is the one command that renews the login.
func (s LoginState) Command() string { return "claude-code databricks login --profile " + s.Profile }

// loginSuppress is how long a dead login is not retried over the network after a failed refresh.
const loginSuppress = 10 * time.Minute

// loginRecord is the on-disk dead-login mark: the failure state and the retry window, never a token.
type loginRecord struct {
	Schema int       `json:"schema"`
	State  string    `json:"state"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
}

const loginStateSchema = 1

var loginStateFormat = schemas.Register(schemas.Format{
	Name: "login-state", Owner: "internal/providers", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "cache", "login-state", "*.json") },
	New:     func() any { return &loginRecord{} },
	Version: loginStateSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// loginMarks holds a mark whose file could not be written, so this process still suppresses retries.
var loginMarks sync.Map // profile name -> loginRecord

// loginNoted is the Until of the mark whose fallback to the service principal this process already reported.
var loginNoted sync.Map // profile name -> time.Time

// readLoginMark is p's dead-login mark: the file (another process's included), else the unwritable-file fallback.
func readLoginMark(name string) (loginRecord, bool) {
	if path := cacheRecordPath("login-state", name); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var rec loginRecord
			if schemas.DecodeOK(loginStateFormat, b, &rec) && rec.State != "" {
				return rec, true
			}
			return loginRecord{}, false
		}
	}
	if v, ok := loginMarks.Load(name); ok {
		return v.(loginRecord), true
	}
	return loginRecord{}, false
}

// LoginStatus is p's remembered dead login, of any age until a login or a successful refresh clears it. It reads one
// small cache file: no network, no vault.
func LoginStatus(p *Provider) (LoginState, bool) {
	if p.Auth.Type != AuthOAuth {
		return LoginState{}, false
	}
	rec, ok := readLoginMark(p.Name)
	if !ok {
		return LoginState{}, false
	}
	return LoginState{Profile: p.Name, State: rec.State, Since: rec.Since}, true
}

// openLoginMark is p's mark while its retry window is still open.
func openLoginMark(p *Provider) (loginRecord, bool) {
	rec, ok := readLoginMark(p.Name)
	return rec, ok && time.Now().Before(rec.Until)
}

// markLoginDead remembers a user refresh that ended needs_login or rejected; any other error leaves marks alone.
func markLoginDead(p *Provider, err error) {
	v := Verdict(err)
	if !v.Condemns() {
		return
	}
	now := time.Now()
	rec := loginRecord{Schema: loginStateSchema, State: string(v), Since: now, Until: now.Add(loginSuppress)}
	if old, ok := readLoginMark(p.Name); ok && old.State == rec.State {
		rec.Since = old.Since
	}
	loginMarks.Delete(p.Name)
	path := cacheRecordPath("login-state", p.Name)
	b, jerr := json.Marshal(rec)
	if path == "" || jerr != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil || paths.AtomicWrite(path, b, 0o600) != nil {
		loginMarks.Store(p.Name, rec)
	}
}

// clearLoginMark forgets p's dead login after a successful mint or a fresh login.
func clearLoginMark(name string) {
	loginMarks.Delete(name)
	loginNoted.Delete(name)
	if path := cacheRecordPath("login-state", name); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s: clear login state failed: %v\n", name, err)
		}
	}
}

// loginRenewalError is what a request gets while the retry window of a dead login is open.
func loginRenewalError(p *Provider, rec loginRecord) error {
	s := LoginState{Profile: p.Name, State: rec.State, Since: rec.Since}
	return &loginRequiredError{err: fmt.Errorf("%s: your login needs renewal (%s since %s): run `%s`",
		p.Name, s.State, s.Since.Local().Format("15:04"), s.Command())}
}

// fallbackToSP serves the paired SP for a dead user login, reporting the fallback once per mark window.
func fallbackToSP(ctx context.Context, p *Provider, force bool) (string, error) {
	v, err := spCredential(ctx, p, force)
	if rec, dead := readLoginMark(p.Name); err == nil && dead {
		if prev, seen := loginNoted.Swap(p.Name, rec.Until); !seen || !prev.(time.Time).Equal(rec.Until) {
			note(ctx, fmt.Sprintf("%s: user login needs renewal; using the service principal (run `%s`)", p.Name,
				LoginState{Profile: p.Name}.Command()))
		}
	}
	return v, err
}

// ProbeUserLogin makes one real user refresh under p's refresh lock and marks or clears the dead login. It is for an
// oauth row with a stored login; err is a transport or other transient failure, which never marks.
func ProbeUserLogin(ctx context.Context, p *Provider) (LoginState, bool, error) {
	if p.Auth.Type != AuthOAuth || !hasUserLogin(p) {
		return LoginState{}, false, nil
	}
	_, _, err := lockedUserMint(ctx, p, true, expiryMargin)
	if err == nil {
		return LoginState{}, false, nil
	}
	if !Verdict(err).Condemns() {
		return LoginState{}, false, err
	}
	st, _ := LoginStatus(p)
	return st, true, nil
}
