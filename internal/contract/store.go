package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Format is the contract state file: one per contract id, written by Go only and trusted on load apart from Validate.
var Format = schemas.Register(schemas.Format{
	Name: "contract", Owner: "internal/contract", Kind: schemas.Record, Version: SchemaVersion,
	Glob: func() string { return filepath.Join(Dir(), "*.json") },
	New:  func() any { return new(Contract) },
	Keep: "a contract's terms and records; a failed one is kept as evidence until the owner removes it",
})

// Dir holds the contract state files, next to the dispatch records.
func Dir() string { return filepath.Join(paths.StateDir(), "contract") }

// Path is id's state file; an id that is not one plain file name is refused.
func Path(id string) (string, error) {
	if !plainName(id) {
		return "", fmt.Errorf("contract: id %q is not a plain file name", id)
	}
	return filepath.Join(Dir(), id+".json"), nil
}

// plainName reports whether s is one non-empty path element on every OS.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\:`)
}

// Load reads id's contract; the decode validates it.
func Load(id string) (Contract, error) {
	p, err := Path(id)
	if err != nil {
		return Contract{}, err
	}
	return load(p)
}

func load(p string) (Contract, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return Contract{}, err
	}
	var c Contract
	if _, err := schemas.Decode(Format, raw, &c); err != nil {
		return Contract{}, fmt.Errorf("contract %s: %w", filepath.Base(p), err)
	}
	return c, nil
}

// Save writes a new contract atomically, 0600, under its lock; it refuses a saved id (Update changes one).
func Save(c Contract) error {
	p, err := Path(c.ID)
	if err != nil {
		return err
	}
	return paths.WithFileLock(p, func() error {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("contract %q: already saved", c.ID), err)
		}
		return write(p, c)
	})
}

func write(p string, c Contract) error {
	if ps := Validate(c); len(ps) > 0 {
		return fmt.Errorf("contract %q: %v", c.ID, ps)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(p, raw, 0o600)
}

// Update loads id, applies fn and saves the result, all under id's lock; fn's error leaves the file as it was.
func Update(id string, fn func(Contract) (Contract, error)) (Contract, error) {
	p, err := Path(id)
	if err != nil {
		return Contract{}, err
	}
	var out Contract
	err = paths.WithFileLock(p, func() error {
		c, err := load(p)
		if err != nil {
			return err
		}
		if out, err = fn(c); err != nil {
			return err
		}
		return write(p, out)
	})
	return out, err
}

// Record appends one term record to id's contract under its lock.
func Record(id string, r TermRecord) error {
	_, err := Update(id, func(c Contract) (Contract, error) { return c.WithRecord(r) })
	return err
}

// Pending lists every saved contract not yet closed or failed, by id; an unreadable file is joined into the error.
func Pending() ([]Contract, error) {
	all, err := List()
	return slices.DeleteFunc(all, func(c Contract) bool { return c.State().Terminal() }), err
}

// List lists every saved contract, by id; an unreadable file is joined into the error.
func List() ([]Contract, error) {
	files, err := filepath.Glob(filepath.Join(Dir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Contract
	var errs []error
	for _, f := range files {
		c, err := load(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Contract) int { return strings.Compare(a.ID, b.ID) })
	return out, errors.Join(errs...)
}

// FailUnreadable replaces id's unreadable file with a failed (unreadable) stub, keeping the bad bytes beside it.
func FailUnreadable(id string) error {
	p, err := Path(id)
	if err != nil {
		return err
	}
	return paths.WithFileLock(p, func() error {
		raw, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err // a read error may be transient: never replace on it
		}
		if _, err := load(p); err == nil || errors.Is(err, schemas.ErrNewer) {
			return nil // readable or future schema: never replace
		}
		scope := Scope{Unit: "unreadable " + id, Kind: KindTask, Own: []string{"-"}}
		c, ps := Open(scope, Party{Role: RoleOwner, Agent: "unknown"}, LimitsFor(KindTask, RoleOwner, Limits{}), Facts{})
		if len(ps) > 0 {
			return fmt.Errorf("contract %q: unreadable stub: %v", id, ps)
		}
		if c, err = c.Start(id); err == nil {
			c, err = c.Fail(ReasonUnreadable)
		}
		if err != nil {
			return err
		}
		if err := paths.AtomicWrite(p+".corrupt", raw, 0o600); err != nil {
			return fmt.Errorf("contract %q: keep the unreadable file: %w", id, err)
		}
		return write(p, c)
	})
}
