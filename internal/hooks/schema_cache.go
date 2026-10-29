package hooks

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// stopFormatSchema is the stop-format stamp cache's current schema version.
const stopFormatSchema = 1

var stopFormatFormat = schemas.Register(schemas.Format{
	Name: "stop-format", Owner: "internal/hooks", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "stop-format", "*.json") },
	New:     func() any { return &stampFile{} },
	Version: stopFormatSchema, Keep: "rebuildable stamps; v0 bare map is dropped, not migrated",
})
