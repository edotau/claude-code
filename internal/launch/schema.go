package launch

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "settings-overlay", Owner: "internal/launch", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "overlay-*.json") },
	Version: 1, Keep: "rewritten on every launch; Claude Code reads it, so no schema field",
})
