package agent

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/schemas"
)

// DispatchSchema versions the dispatch record file.
const DispatchSchema = 1

var (
	dispatchFormat = schemas.Register(schemas.Format{
		Name: "dispatch-record", Owner: "internal/agent", Kind: schemas.Record, Version: DispatchSchema,
		Glob:    func() string { return filepath.Join(DispatchDir(), "*.json") },
		New:     func() any { return new(Record) },
		Clean:   "dispatch-log",
		Migrate: schemas.SameShape,
	})

	_ = schemas.Register(schemas.Format{
		Name: "ask-envelope", Owner: "internal/agent", Kind: schemas.Wire, Version: EnvelopeSchema,
		New: func() any { return new(Envelope) },
	})
)
