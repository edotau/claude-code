package settings

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "vendor-baseline", Owner: "internal/settings", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "vendor-baseline", "*") },
	Version: 1, Keep: "raw bytes last written to a vendor file; reseeded when missing",
})
