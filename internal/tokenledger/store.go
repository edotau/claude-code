// Package tokenledger is the per-token ledger the proxy appends to and usage and the status line read; stdlib-light for the proxy.
package tokenledger

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
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
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	e.Schema = schemaVersion
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if endsMidLine(f) {
		// A prior torn write left no newline; start this entry on its own line.
		b = append([]byte{'\n'}, b...)
	}
	_, _ = f.Write(b)
}

// endsMidLine reports whether the file is non-empty and its last byte is not a newline.
func endsMidLine(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
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
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if _, err := schemas.Decode(format, sc.Bytes(), &e); err == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
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
