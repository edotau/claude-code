package memory

import "time"

// Version is an opaque per-entry token issued by the store; callers pass it back untouched and never parse it.
type Version string

// Versions maps an entry ref to its Version; "" marks a ref that is unknown or ambiguous, so not writable.
type Versions map[string]Version

// bankStore is the one seam typed writers use; files today, an engine later.
type bankStore interface {
	Load() (map[string]Doc, Versions, error)
	Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) // all or nothing
}

// fileBankStore keeps the bank in its markdown files; locked means the caller already holds the bank lock.
type fileBankStore struct{ locked bool }

func newBankStore(locked bool) bankStore { return fileBankStore{locked: locked} }

func (fileBankStore) Load() (map[string]Doc, Versions, error) {
	docs, _, err := loadBatchDocs()
	if err != nil {
		return nil, nil, err
	}
	versions := Versions{}
	for _, name := range bankFiles {
		for _, e := range docs[name].Entries {
			ref := e.refOf()
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
