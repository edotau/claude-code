package worktree

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/schemas"
)

// operationSchema versions worktree-merge-op.
const operationSchema = 1

var operationFormat = schemas.Register(schemas.Format{
	Name: "worktree-merge-op", Owner: "internal/worktree", Kind: schemas.Record, Version: operationSchema,
	Glob:    func() string { return filepath.Join(operationsDir(), "*.json") },
	New:     func() any { return new(Operation) },
	Keep:    "resume record for an interrupted agent merge",
	Migrate: schemas.SameShape,
})
