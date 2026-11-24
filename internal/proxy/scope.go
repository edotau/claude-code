package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Scope is a scoped proxy's record: scopes/<team>-<id>.json, written while it serves, removed when it closes.
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
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._-", rune(c))) {
			return false
		}
	}
	return true
}

func scopeFile(team, id string) string { return filepath.Join(ScopesDir(), team+"-"+id+".json") }

func checkScopeParts(team, id string) error {
	if !ValidScopePart(team) || !ValidScopePart(id) {
		return fmt.Errorf("proxy scope: invalid team %q or id %q", team, id)
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
	if err := paths.WithFileLock(file, func() error { return paths.AtomicWrite(file, data, 0o600) }); err != nil {
		return nil, err
	}
	return func() {
		_ = paths.WithFileLock(file, func() error { // a replacement's fresh record must not vanish mid-check
			if cur, err := ReadScope(sc.Team, sc.ID); err == nil && cur.PID == sc.PID && cur.Started.Equal(sc.Started) {
				_ = os.Remove(file)
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
