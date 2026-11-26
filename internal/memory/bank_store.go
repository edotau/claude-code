package memory

import (
	"fmt"
	"maps"
	"slices"
	"time"
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

// fileBankStore keeps the bank in its markdown files; locked means the caller already holds the bank lock.
type fileBankStore struct{ locked bool }

func newBankStore(locked bool) BankStore { return fileBankStore{locked: locked} }

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
