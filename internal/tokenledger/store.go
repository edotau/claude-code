// Package tokenledger is the per-token ledger the proxy appends to and usage and the status line read.
package tokenledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// Entry is one metered proxy request; never content, keys or headers. Older lines lack the attribution fields
// and carry the profile name in Provider; ProfileName reads both.
type Entry struct {
	Schema    int       `json:"schema"`
	Ts        time.Time `json:"ts"`
	Profile   string    `json:"profile,omitempty"`
	Provider  string    `json:"provider"`
	Agent     string    `json:"agent,omitempty"`
	Session   string    `json:"session,omitempty"`
	Model     string    `json:"model"`
	Input     int       `json:"input"`
	Output    int       `json:"output"`
	CacheRead int       `json:"cache_read"`
}

// Path is <config dir>/state/usage/ledger.jsonl.
func Path() string {
	return filepath.Join(paths.ConfigDir(), "state", "usage", "ledger.jsonl")
}

// Append appends one JSON line; best-effort — a write failure never fails the caller's request.
func Append(e Entry) {
	e.Schema = schemaVersion
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = paths.AppendLineRepair(Path(), b)
}

// Read reads every entry, oldest first; a missing ledger file is empty, not an error.
func Read() ([]Entry, error) {
	f, err := os.Open(Path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	err = textutil.EachLineErr(f, func(line []byte) {
		var e Entry
		if _, err := schemas.Decode(format, line, &e); err == nil {
			out = append(out, e)
		}
	})
	return out, err
}

// ProfileName is the registry row that served the entry; pre-attribution lines stored it in Provider.
func (e Entry) ProfileName() string {
	if e.Profile != "" {
		return e.Profile
	}
	return e.Provider
}

// TotalsBySession sums the tokens of every entry stamped with the parent session id; "" matches nothing.
func TotalsBySession(id string) (in, out, cacheRead int) {
	if id == "" {
		return 0, 0, 0
	}
	entries, _ := Read()
	for _, e := range entries {
		if e.Session == id {
			in, out, cacheRead = in+e.Input, out+e.Output, cacheRead+e.CacheRead
		}
	}
	return in, out, cacheRead
}

// schemaVersion is the usage ledger's current schema version.
const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "usage-ledger", Owner: "internal/tokenledger", Kind: schemas.Log,
	Glob:    Path,
	New:     func() any { return &Entry{} },
	Version: schemaVersion, Keep: "usage history is the ledger of record", Migrate: schemas.SameShape,
})
