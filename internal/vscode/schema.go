package vscode

import (
	"github.com/edotau/claude-code/internal/schemas"
)

// stateSchema versions vscode-serve-runtime.
const stateSchema = 1

var serveFormat = schemas.Register(schemas.Format{
	Name: "vscode-serve-runtime", Owner: "internal/vscode", Kind: schemas.Record, Version: stateSchema,
	Glob:    func() string { return stateFile() },
	New:     func() any { return new(State) },
	Keep:    "runtime state; removed when the daemon exits",
	Migrate: schemas.SameShape,
})
