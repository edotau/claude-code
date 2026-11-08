package agent

import "github.com/edotau/claude-code/internal/schemas"

var _ = schemas.Register(schemas.Format{
	Name: "live-request", Owner: "internal/agent", Kind: schemas.Wire, Version: LiveSchema,
	New: func() any { return new(LiveRequest) },
})
