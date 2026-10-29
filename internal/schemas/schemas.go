// Package schemas is the registry of versioned on-disk and wire formats; owners register their own rows.
package schemas

import (
	"errors"
	"fmt"
	"sort"
	"sync"
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
