package memory

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/schemas"
)

// journalSchema versions memory-journal.
const journalSchema = 1

var journalFormat = schemas.Register(schemas.Format{
	Name: "memory-journal", Owner: "internal/memory", Kind: schemas.Record, Version: journalSchema,
	Glob:    func() string { return filepath.Join(JournalDir(), "*.json") },
	New:     func() any { return new(journal) },
	Keep:    "a pending journal is replayed by the next commit; a newer one is never replayed",
	Migrate: schemas.SameShape,
})
