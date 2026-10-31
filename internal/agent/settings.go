package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

// settingKind is a field's edit affordance.
type settingKind int

const (
	KindBool   settingKind = iota // "true"/"false"
	KindEnum                      // one of Values
	KindText                      // free text (e.g. a duration string)
	KindList                      // comma-separated strings
	KindNumber                    // a float, as text
)

// Field is one typed routing knob: read/write providers.Routing, rendered as one configure row.
type Field struct {
	Key, Label, Help string
	Kind             settingKind
	Values           []string // KindEnum only
	Get              func(providers.Routing) string
	Set              func(*providers.Routing, string) error // "" resets to default (removes the overlay field)
	// IsSet reports whether the vault overlay's own routing object (providers.RoutingOverlay, never the
	// merged registry) sets this field — the provenance check: "vault" when true, else "default".
	IsSet func(providers.Routing) bool
}

// Module is one vendor's routing fields plus the env it contributes at launch.
type Module interface {
	Name() string
	Fields() []Field
	Env(r providers.Routing) map[string]string
}

// Modules lists every registered module, in display/merge order: the core routing knobs first, then
// each vendor's own.
func Modules() []Module {
	return []Module{routingModule{}, opencodeModule{}}
}
