package usageledger

import "github.com/edotau/claude-code/internal/schemas"

// schemaVersion is the usage ledger's current schema version.
const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "usage-ledger", Owner: "internal/usageledger", Kind: schemas.Log,
	Glob:    Path,
	New:     func() any { return &Entry{} },
	Version: schemaVersion, Keep: "usage history is the ledger of record", Migrate: schemas.SameShape,
})
