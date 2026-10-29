package setup

import (
	"github.com/edotau/claude-code/internal/schemas"
)

// stateSchema versions setup-state.
const stateSchema = 1

var setupFormat = schemas.Register(schemas.Format{
	Name: "setup-state", Owner: "internal/setup", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "last adopt record",
	Migrate: schemas.SameShape,
})
