package audit

import "github.com/edotau/claude-code/internal/schemas"

// schemaVersion is the audit log's current schema version.
const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "audit", Owner: "internal/audit", Kind: schemas.Log,
	Glob:    func() string { return File() + "*" },
	New:     func() any { return &Event{} },
	Version: schemaVersion, Keep: "audit trail is kept; rotated at 1 MiB", Migrate: schemas.SameShape,
})
