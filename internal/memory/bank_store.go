package memory

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Version is an opaque per-entry token issued by the store; callers pass it back untouched and never parse it.
type Version string

// Versions maps an entry ref to its Version; "" marks a ref that is unknown or ambiguous, so not writable.
type Versions map[string]Version

// ConflictError is a typed Apply refusal: op Op named Ref at a Version other than the base it was read at.
type ConflictError struct {
	Op            int
	Ref           string
	Base, Current Version
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("op %d: %s changed since read (base %q, now %q)", e.Op, e.Ref, e.Base, e.Current)
}

// BankStore is the db seam typed writers use.
type BankStore interface {
	Load() (map[string]Doc, Versions, error)
	Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) // all or nothing
}

// OpenBankStore is nil when no db store is linked; the close func releases the db.
var OpenBankStore func(locked bool) (BankStore, func() error, error)

var errNoDBStore = errors.New("memory: no db store is linked")

// truthPath is the retired truth file of the files era; only retireFilesTruth reads it.
func truthPath() string { return filepath.Join(paths.StateDir(), "memory", "truth") }

// storedTruthNotDB is true only for a truth file read successfully that is not "db"; missing or unreadable is false.
func storedTruthNotDB() bool {
	b, err := os.ReadFile(truthPath())
	return err == nil && strings.TrimSpace(string(b)) != "db"
}

// retireFilesTruth rewrites a stored truth other than "db" to "db"; the caller has just run a conflict-free guard.
func retireFilesTruth() {
	if !storedTruthNotDB() {
		return
	}
	if err := paths.AtomicWrite(truthPath(), []byte("db\n"), 0o644); err != nil { // never fails the caller
		fmt.Fprintf(os.Stderr, "memory: retire the files truth: %v\n", err)
	}
}

// NewBankStore is the db store; locked means the caller holds the bank lock, else Apply takes it.
func NewBankStore(locked bool) BankStore { return newBankStore(locked) }

func newBankStore(locked bool) BankStore { return dbBankStore{locked: locked} }

// dbBankStore opens the db store for each call and closes it after, so callers need no close func.
type dbBankStore struct{ locked bool }

func (s dbBankStore) with(f func(BankStore) error) error {
	if OpenBankStore == nil {
		return errNoDBStore
	}
	st, closeFn, err := OpenBankStore(s.locked)
	if err != nil {
		return fmt.Errorf("open db store: %w", err)
	}
	err = f(st)
	if cerr := closeFn(); cerr != nil { // a committed batch must not look retryable
		fmt.Fprintf(os.Stderr, "memory: closing db store: %v\n", cerr)
	}
	return err
}

func (s dbBankStore) Load() (docs map[string]Doc, v Versions, err error) {
	err = s.with(func(st BankStore) (e error) { docs, v, e = st.Load(); return })
	return
}

// Apply commits b and exports the Markdown under one bank lock (taken here unless held); flock is not reentrant.
func (s dbBankStore) Apply(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	if s.locked {
		return s.applyLocked(b, base, now)
	}
	err = withBankLock(BankLockWait, func() (e error) { rs, e = s.applyLocked(b, base, now); return })
	return
}

func (s dbBankStore) applyLocked(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	locked := dbBankStore{locked: true}
	if err = locked.with(func(st BankStore) (e error) { rs, e = st.Apply(b, base, now); return }); err != nil {
		return
	}
	clean, xerr := exportAfterApply()
	if xerr != nil {
		return rs, &AfterCommitError{&committedErr{fmt.Errorf("batch committed in the db; markdown export: %w", xerr)}}
	}
	if clean {
		retireFilesTruth() // the guard imported the Markdown a files truth left behind, with no conflict kept apart
	}
	return
}

// exportAfterApply runs the guard after a db commit under the held bank lock; clean means it ran with no conflict.
func exportAfterApply() (clean bool, err error) {
	if OpenExportDB == nil {
		return false, nil
	}
	edb, closeDB, err := OpenExportDB()
	if err != nil {
		return false, err
	}
	indexErr, conflicts, err := guardPass(edb, paths.MemoryStoreDir())
	return len(conflicts) == 0 && err == nil && indexErr == nil, errors.Join(indexErr, err, closeDB())
}

// ApplyDocs runs b over a copy of docs; a failed op fails the batch, so a store writes nothing.
func ApplyDocs(docs map[string]Doc, b Batch, now time.Time) (map[string]Doc, []OpResult, error) {
	b.Ops = readAsLoaded(docs, slices.Clone(b.Ops))
	if err := b.Validate(); err != nil {
		return nil, nil, err
	}
	out := maps.Clone(docs)
	results := runBatchOps(out, b, now)
	if n := countFailedOps(results); n > 0 {
		return out, results, fmt.Errorf("%d of %d ops failed: nothing written", n, len(b.Ops))
	}
	return out, results, nil
}

// readAsLoaded gives an edit with no Read the contentRef of its entry in docs, for a store that checked base itself.
func readAsLoaded(docs map[string]Doc, ops []Op) []Op {
	for i, o := range ops {
		name, _ := routeBatchOp(docs, o)
		r, ok := ParseRef(o.ID)
		if !o.editsEntry() || o.Read != "" || name == "" || !ok || r.Derived {
			continue
		}
		for _, e := range docs[name].Entries {
			if e.matches(r) {
				ops[i].Read = contentRef(e.block())
			}
		}
	}
	return ops
}
