package vendors

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var (
	_ = schemas.Register(schemas.Format{
		Name: "opencode-overlay", Owner: "internal/vendors", Kind: schemas.Derived,
		Glob:    func() string { return filepath.Join(opencodeOverlayDir(), "*.json") },
		Version: 1, Keep: "re-rendered from the template on every launch; opencode reads it, so no schema field",
	})
	_ = schemas.Register(schemas.Format{
		Name: "codex-catalog", Owner: "internal/vendors", Kind: schemas.Derived,
		Glob:    func() string { return filepath.Join(paths.StateDir(), "codex", "catalog-*.json") },
		Version: 1, Keep: "rebuilt from the provider catalog; codex reads it, so no schema field",
	})
)
