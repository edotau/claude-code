package hooks

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// codexRouteSchema is the codex routing audit's current schema version.
const codexRouteSchema = 1

var codexRouteFormat = schemas.Register(schemas.Format{
	Name: "codex-routing", Owner: "internal/hooks", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "codex", "routing-*.jsonl") },
	New:     func() any { return &codexRouteRecord{} },
	Version: codexRouteSchema, Clean: "codex-audit", Migrate: schemas.SameShape,
})
