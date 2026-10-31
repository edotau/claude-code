package project

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "projections", Owner: "internal/agent/project", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.ConfigDir(), "state", "projections", "*") },
	Version: 1, Clean: "projections",
})
