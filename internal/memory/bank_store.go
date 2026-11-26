package memory

import (
	"errors"
	"fmt"
	"io/fs"
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

// BankStore is the one seam typed writers use; files today, an engine later.
type BankStore interface {
	Load() (map[string]Doc, Versions, error)
	Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) // all or nothing
}

// ShadowImport is nil when no shadow store is linked; changed means the divergent set moved since the last import.
var ShadowImport func() (diverged []string, changed bool, err error)

// OpenBankStore is nil when no db store is linked; the close func releases the db.
var OpenBankStore func(locked bool) (BankStore, func() error, error)

// ExportBeforeFlip runs under the bank lock (not reentrant) when SetTruth leaves "db"; nil refuses; TODO(7.0): wire it.
var ExportBeforeFlip func() error

const (
	truthFiles = "files"
	truthDB    = "db"
)

func truthPath() string { return filepath.Join(paths.StateDir(), "memory", "truth") }

// TruthErr reports "files" or "db"; a missing file is "files", an unreadable or unknown one an error.
func TruthErr() (string, error) {
	b, err := os.ReadFile(truthPath())
	if errors.Is(err, fs.ErrNotExist) {
		return truthFiles, nil
	}
	if err != nil {
		return "", fmt.Errorf("memory truth: %w", err)
	}
	if v := strings.TrimSpace(string(b)); v == truthFiles || v == truthDB {
		return v, nil
	}
	return "", fmt.Errorf("memory truth: unknown value %q in %s", strings.TrimSpace(string(b)), truthPath())
}

// Truth is TruthErr for display: an error reads as "files".
func Truth() string {
	t, err := TruthErr()
	if err != nil {
		return truthFiles
	}
	return t
}

// SetTruth records the source of truth under the bank lock; leaving "db" only flips after a successful export.
func SetTruth(t string) error {
	if t != truthFiles && t != truthDB {
		return fmt.Errorf("truth %q: want %q or %q", t, truthFiles, truthDB)
	}
	return WithBankLock(BankLockWait, func() error {
		cur, err := TruthErr()
		if err != nil {
			return err
		}
		if t == truthFiles && cur == truthDB {
			if ExportBeforeFlip == nil {
				return errors.New("db export unavailable: cannot flip back without losing db-only changes")
			}
			if err := ExportBeforeFlip(); err != nil {
				return fmt.Errorf("export before flip to files: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(truthPath()), 0o755); err != nil {
			return err
		}
		return paths.AtomicWrite(truthPath(), []byte(t+"\n"), 0o644)
	})
}

// fileBankStore keeps the bank in its markdown files; locked means the caller already holds the bank lock.
type fileBankStore struct{ locked bool }

// failedStore refuses every call with err, so a truth that cannot be read never falls back to files.
type failedStore struct{ err error }

func (f failedStore) Load() (map[string]Doc, Versions, error) { return nil, nil, f.err }
func (f failedStore) Apply(Batch, Versions, time.Time) ([]OpResult, error) {
	return nil, f.err
}

// truthStore reads the truth per call, in Apply under the bank lock, so a flip never writes to the old store.
type truthStore struct{ locked bool }

func newBankStore(locked bool) BankStore { return truthStore{locked: locked} }

func (s truthStore) Load() (map[string]Doc, Versions, error) { return pick(s.locked).Load() }

func (s truthStore) Apply(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	if s.locked {
		return pick(true).Apply(b, base, now)
	}
	err = withFlatLock(BankLockWait, func() (e error) { rs, e = pick(true).Apply(b, base, now); return })
	return
}

// pick is the store for the current truth, a failing one on a bad truth; locked: the caller holds the bank lock.
func pick(locked bool) BankStore {
	switch t, err := TruthErr(); {
	case err != nil:
		return failedStore{err}
	case t == truthDB && OpenBankStore == nil:
		return failedStore{errors.New("memory truth is db but no db store is linked")}
	case t == truthDB:
		return dbBankStore{locked: locked}
	}
	return fileBankStore{locked: locked}
}

// dbBankStore opens the db store for each call and closes it after, so callers need no close func.
type dbBankStore struct{ locked bool }

func (s dbBankStore) with(f func(BankStore) error) error {
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

func (s dbBankStore) Apply(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	err = s.with(func(st BankStore) (e error) { rs, e = st.Apply(b, base, now); return })
	return
}

func (fileBankStore) Load() (map[string]Doc, Versions, error) {
	docs, _, err := loadBatchDocs()
	if err != nil {
		return nil, nil, err
	}
	versions := Versions{}
	for _, name := range bankFiles {
		for _, e := range docs[name].Entries {
			ref := e.Ref()
			if _, seen := versions[ref]; seen {
				versions[ref] = ""
			} else {
				versions[ref] = Version(contentRef(e.block()))
			}
		}
	}
	return docs, versions, nil
}

func (s fileBankStore) Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) {
	if s.locked {
		return applyBatchLocked(b, base, now)
	}
	return ApplyBatch(b, base, now)
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
