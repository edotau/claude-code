package wire

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var (
	_ = schemas.Register(schemas.Format{
		Name: "dispatch-events", Owner: "internal/agent/wire", Kind: schemas.Log, Version: EventSchema,
		Glob:  func() string { return filepath.Join(paths.DispatchDir(), "*"+paths.DispatchEventsSuffix) },
		New:   func() any { return new(Event) },
		Clean: "dispatch-log",
	})

	_ = schemas.Register(schemas.Format{
		Name: "ask-envelope", Owner: "internal/agent/wire", Kind: schemas.Wire, Version: EnvelopeSchema,
		New: func() any { return new(Envelope) },
	})

	_ = schemas.Register(schemas.Format{
		Name: "live-request", Owner: "internal/agent/wire", Kind: schemas.Wire, Version: LiveSchema,
		New: func() any { return new(LiveRequest) },
	})
)
