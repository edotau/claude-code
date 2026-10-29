package proxy

import (
	"github.com/edotau/claude-code/internal/schemas"
)

// stateSchema versions router-runtime.
const stateSchema = 1

var routerFormat = schemas.Register(schemas.Format{
	Name: "router-runtime", Owner: "internal/proxy", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "runtime state; removed when the daemon exits",
	Migrate: schemas.SameShape,
})
