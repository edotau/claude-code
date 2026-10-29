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

var routerSessionsFormat = schemas.Register(schemas.Format{
	Name: "router-sessions", Owner: "internal/proxy", Kind: schemas.Record, Version: 1,
	Glob:    sessionsFile,
	New:     func() any { return new(sessionState) },
	Keep:    "process-bound session token hashes; retained across proxy restarts and pruned when expired or revoked",
	Migrate: schemas.SameShape,
})
