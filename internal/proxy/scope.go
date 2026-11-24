package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
)

// Scope is a scoped proxy's record: scopes/<team>@<id>.json, written while it serves, removed when it closes.
type Scope struct {
	Schema  int       `json:"schema"`
	Team    string    `json:"team"`
	ID      string    `json:"id"`
	PID     int       `json:"pid"`
	Start   string    `json:"start"` // the process start-time identity (proc.StartTime), guards pid reuse
	Port    int       `json:"port"`
	Build   string    `json:"build"`
	Started time.Time `json:"started"`
}

const (
	scopeSchema   = 1
	scopeMaxBytes = 64 << 10
)

var scopeFormat = schemas.Register(schemas.Format{
	Name: "proxy-scope", Owner: "internal/proxy", Kind: schemas.Record, Version: scopeSchema,
	Glob:    func() string { return filepath.Join(ScopesDir(), "*.json") },
	New:     func() any { return new(Scope) },
	Keep:    "runtime state; removed when the scoped proxy closes",
	Migrate: schemas.SameShape,
})

// ScopesDir is the directory of scoped-proxy records.
func ScopesDir() string { return filepath.Join(paths.StateDir(), "proxy", "scopes") }

// ValidScopePart reports whether s is 1..64 file-name-safe characters starting alphanumeric.
func ValidScopePart(s string) bool {
	if len(s) < 1 || len(s) > 64 || !isAlnum(s[0]) {
		return false
	}
	return !slices.ContainsFunc([]byte(s), func(c byte) bool { return !isAlnum(c) && !strings.ContainsRune("._-", rune(c)) })
}

// isAlnum is lower-case only: file names that differ by case would share a file on a case-insensitive filesystem.
func isAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// scopeFile joins with "@", outside the part alphabet, so two pairs never share a file.
func scopeFile(team, id string) string { return filepath.Join(ScopesDir(), team+"@"+id+".json") }

// scopeLive reports whether sc's process may still run: only a gone pid or a changed start time proves it dead.
func scopeLive(sc Scope) bool {
	start, err := proc.StartTime(sc.PID)
	if err != nil {
		return !errors.Is(err, proc.ErrNoProcess)
	}
	return start == sc.Start
}

func checkScopeParts(team, id string) error {
	if !ValidScopePart(team) || !ValidScopePart(id) {
		return fmt.Errorf("proxy scope: invalid team %q or id %q", team, id)
	}
	return nil
}

// scopeBlocked is non-nil when the record may belong to a live proxy; a missing or corrupt one does not block.
func scopeBlocked(team, id string) error {
	cur, err := ReadScope(team, id)
	switch {
	case err == nil && scopeLive(cur):
		return fmt.Errorf("proxy scope: %s@%s is already served by pid %d", team, id, cur.PID)
	case errors.Is(err, schemas.ErrNewer), errors.Is(err, fs.ErrPermission):
		return err // unreadable is not dead: a newer build or another user may own it
	}
	return nil
}

// WriteScope publishes sc and returns a cleanup that removes the file only while it is still this record.
func WriteScope(sc Scope) (func(), error) {
	if err := checkScopeParts(sc.Team, sc.ID); err != nil {
		return nil, err
	}
	sc.Schema = scopeSchema
	data, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	file := scopeFile(sc.Team, sc.ID)
	err = paths.WithFileLock(file, func() error {
		if err := scopeBlocked(sc.Team, sc.ID); err != nil {
			return err
		}
		return paths.AtomicWrite(file, data, 0o600)
	})
	if err != nil {
		return nil, err
	}
	return func() {
		_ = paths.WithFileLock(file, func() error { // a replacement's fresh record must not vanish mid-check
			if cur, err := ReadScope(sc.Team, sc.ID); err == nil && cur.PID == sc.PID && cur.Started.Equal(sc.Started) {
				if err := os.Remove(file); err != nil {
					fmt.Fprintf(os.Stderr, "proxy scope: removing %s: %v\n", file, err)
				}
			}
			return nil
		})
	}, nil
}

// ReadScope returns the record of team and id.
func ReadScope(team, id string) (Scope, error) {
	if err := checkScopeParts(team, id); err != nil {
		return Scope{}, err
	}
	sc, err := readScopeFile(scopeFile(team, id))
	if err != nil {
		return Scope{}, err
	}
	if sc.Team != team || sc.ID != id {
		return Scope{}, errors.New("proxy scope: record does not match its file name")
	}
	return sc, nil
}

func readScopeFile(file string) (Scope, error) {
	var sc Scope
	f, err := os.Open(file)
	if err != nil {
		return sc, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, scopeMaxBytes))
	if err != nil {
		return sc, err
	}
	_, err = schemas.Decode(scopeFormat, b, &sc)
	return sc, err
}

// ListScopes returns every decodable record, sorted by team then id; a missing directory is empty.
func ListScopes() ([]Scope, error) {
	files, err := filepath.Glob(filepath.Join(ScopesDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Scope
	for _, f := range files {
		if sc, err := readScopeFile(f); err == nil {
			out = append(out, sc)
		}
	}
	slices.SortFunc(out, func(a, b Scope) int {
		if c := strings.Compare(a.Team, b.Team); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}
