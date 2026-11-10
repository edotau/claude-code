// Package schemas is the registry of versioned on-disk and wire formats; owners register their own rows.
package schemas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Kind classifies a format by how it is stored.
type Kind string

const (
	Record  Kind = "record"  // one JSON document per file
	Log     Kind = "log"     // append-only JSON lines
	Cache   Kind = "cache"   // rebuildable JSON file
	Store   Kind = "store"   // opaque store with its own versioning (probed)
	Wire    Kind = "wire"    // never on disk
	Derived Kind = "derived" // regenerated from other state
)

// Format is one registered row.
type Format struct {
	Name, Owner string
	Kind        Kind
	Glob        func() string                              // nil only for Wire
	New         func() any                                 // nil for Store/Derived
	Version     int                                        // current schema version
	Clean, Keep string                                     // Keep required when Clean == ""
	Migrate     func(from int, raw []byte) ([]byte, error) // one step from -> from+1
	Rewrite     bool                                       // MigrateAll rewrites on disk (locked writers only)
	Probe       func() (int, error)                        // Store kinds
}

var (
	// ErrNewer means the data was written by a newer build than this one.
	ErrNewer = errors.New("schema is newer than this build")
	// ErrNoMigration means an older version has no migration step to the current one.
	ErrNoMigration = errors.New("no migration from this schema version")

	mu   sync.Mutex
	rows = map[string]Format{}
)

// Register adds a row and returns it; a duplicate Name or invalid row is a programmer error and panics.
func Register(f Format) Format {
	switch {
	case f.Name == "":
		panic("schemas: format without a name")
	case f.Kind != Wire && f.Kind != Store && f.Glob == nil:
		panic(fmt.Sprintf("schemas: %s needs a Glob", f.Name))
	case f.Clean == "" && f.Keep == "" && f.Kind != Wire:
		panic(fmt.Sprintf("schemas: %s needs Clean or Keep", f.Name))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := rows[f.Name]; dup {
		panic("schemas: duplicate format " + f.Name)
	}
	rows[f.Name] = f
	return f
}

// All returns the registered rows sorted by name.
func All() []Format {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Format, 0, len(rows))
	for _, f := range rows {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// peek returns the document's "schema" number; a missing field is version 0.
func peek(raw []byte) (int, error) {
	var p struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, errors.New("document is null, not an object")
	}
	if p.Schema == nil {
		return 0, nil
	}
	if *p.Schema < 0 {
		return 0, fmt.Errorf("negative schema version %d", *p.Schema)
	}
	return *p.Schema, nil
}

// upgrade applies Migrate step by step from the document's version to f.Version.
func upgrade(f Format, raw []byte) (out []byte, from int, err error) {
	from, err = peek(raw)
	if err != nil {
		return nil, 0, err
	}
	if from > f.Version {
		return nil, from, fmt.Errorf("%s v%d > v%d: %w", f.Name, from, f.Version, ErrNewer)
	}
	out = raw
	for v := from; v < f.Version; v++ {
		if f.Migrate == nil {
			return nil, from, fmt.Errorf("%s v%d -> v%d: %w", f.Name, v, v+1, ErrNoMigration)
		}
		if out, err = f.Migrate(v, out); err != nil {
			return nil, from, fmt.Errorf("%s v%d -> v%d: %w", f.Name, v, v+1, err)
		}
	}
	return out, from, nil
}

// Decode unmarshals raw into dst, migrating older versions step by step; from is the version found.
func Decode(f Format, raw []byte, dst any) (from int, err error) {
	out, from, err := upgrade(f, raw)
	if err != nil {
		return from, err
	}
	return from, json.Unmarshal(out, dst)
}

// SameShape is a Migrate step for a version bump that only adds the "schema" stamp.
func SameShape(from int, raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("document is null, not an object")
	}
	n, err := json.Marshal(from + 1)
	if err != nil {
		return nil, err
	}
	m["schema"] = n
	return json.Marshal(m)
}

// DecodeOK reports whether raw decodes at the current version; an unparsable or newer-schema document is a miss.
func DecodeOK(f Format, raw []byte, dst any) bool {
	_, err := Decode(f, raw, dst)
	return err == nil
}

// Stale is one file holding data older than its format's current version.
type Stale struct {
	Format  string
	Path    string
	From    int // oldest version found in the file
	To      int
	Records int // stale records (lines for a log, 1 otherwise)
	Rewrite bool
	Err     string `json:",omitempty"`
}

// docs splits a file into JSON documents: one per line for logs, the whole file otherwise.
func docs(f Format, data []byte) [][]byte {
	if f.Kind != Log {
		return [][]byte{data}
	}
	var out [][]byte
	for l := range bytes.SplitSeq(data, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func onDisk(f Format) bool {
	return f.Glob != nil && f.Kind == Record || f.Kind == Log
}

func scan(f Format) []Stale {
	if !onDisk(f) {
		return nil
	}
	files, _ := filepath.Glob(f.Glob())
	var out []Stale
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := Stale{Format: f.Name, Path: p, To: f.Version, Rewrite: f.Rewrite, From: f.Version}
		for _, d := range docs(f, data) {
			v, err := peek(d)
			if err != nil || v >= f.Version {
				continue
			}
			s.Records++
			s.From = min(s.From, v)
		}
		if s.Records > 0 {
			out = append(out, s)
		}
	}
	return out
}

// Pending lists Record and Log files older than their registered version; a cache mismatch is a miss, not stale.
// A row with Rewrite false is read via migration (informational).
func Pending() []Stale {
	var out []Stale
	for _, f := range All() {
		out = append(out, scan(f)...)
	}
	return out
}

// MigrateAll rewrites stale files of Rewrite rows under a file lock; dryRun only reports. Other rows migrate on read.
func MigrateAll(dryRun bool) ([]Stale, error) {
	var done []Stale
	var firstErr error
	for _, f := range All() {
		if !f.Rewrite {
			continue
		}
		records := map[int]int{} // from -> records migrated
		for _, s := range scan(f) {
			if dryRun {
				done = append(done, s)
				continue
			}
			if err := rewrite(f, &s); err != nil {
				s.Err = err.Error()
				if firstErr == nil {
					firstErr = err
				}
			} else {
				records[s.From] += s.Records
			}
			done = append(done, s)
		}
		for from, n := range records {
			LogMigration(f.Name, from, f.Version, n)
		}
	}
	return done, firstErr
}

func rewrite(f Format, s *Stale) error {
	return paths.WithFileLock(s.Path, func() error {
		data, err := os.ReadFile(s.Path) // re-read under the lock
		if err != nil {
			return err
		}
		var out [][]byte
		n := 0
		for _, d := range docs(f, data) {
			if v, perr := peek(d); perr == nil && v < f.Version {
				m, _, err := upgrade(f, d)
				if err != nil {
					return err
				}
				d = m
				n++
			}
			out = append(out, d)
		}
		if f.Kind != Log {
			if err := paths.AtomicWrite(s.Path, out[0], 0o600); err != nil {
				return err
			}
		} else if err := paths.AtomicWrite(s.Path, append(bytes.Join(out, []byte("\n")), '\n'), 0o600); err != nil {
			return err
		}
		s.Records = n
		return nil
	})
}

var _ = Register(Format{
	Name: "migrations", Owner: "schemas", Kind: Log, Glob: LedgerPath, Version: 1,
	Keep: "tiny audit trail of schema upgrades",
})

type ledgerRow struct {
	Schema  int    `json:"schema"`
	At      string `json:"at"`
	Format  string `json:"format"`
	From    int    `json:"from"`
	To      int    `json:"to"`
	Records int    `json:"records"`
}

// LedgerPath is the migration ledger file.
func LedgerPath() string { return filepath.Join(paths.StateDir(), "migrations.jsonl") }

// LogMigration appends one ledger line; fail-open: a write error never reaches the caller.
func LogMigration(format string, from, to, records int) {
	at := time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(ledgerRow{Schema: 1, At: at, Format: format, From: from, To: to, Records: records})
	if err != nil {
		return
	}
	p := LedgerPath()
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}
