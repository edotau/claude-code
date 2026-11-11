package configure

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// routingKind is a field's edit affordance.
type routingKind int

const (
	RoutingBool   routingKind = iota // "true"/"false"
	RoutingEnum                      // one of Values
	RoutingText                      // free text (e.g. a duration string)
	RoutingList                      // comma-separated strings
	RoutingNumber                    // a float, as text
)

// RoutingField is one typed routing knob: read/write providers.Routing, rendered as one configure row.
type RoutingField struct {
	Key    string
	Kind   routingKind
	Values []string // RoutingEnum only
	Get    func(providers.Routing) string
	Set    func(*providers.Routing, string) error // "" resets to default (removes the overlay field)
	// IsSet reports whether the vault overlay's own routing object (providers.RoutingOverlay, never the
	// merged registry) sets this field — the provenance check: "vault" when true, else "default".
	IsSet func(providers.Routing) bool
}

// Module is one agent's routing fields.
type Module interface {
	Name() string
	Fields() []RoutingField
}

// Modules lists every registered module, in display/merge order: the core routing knobs first, then
// each agent's own.
func Modules() []Module {
	return []Module{routingModule{}, opencodeModule{}}
}

// routingModule is the core module: the composite-score weight and the served-model switch.
type routingModule struct{}

func (routingModule) Name() string { return "routing" }

func (routingModule) Fields() []RoutingField {
	return []RoutingField{weightsPriceField(), servedAwareField()}
}

func weightsPriceField() RoutingField {
	return RoutingField{
		Key:  "weights.price",
		Kind: RoutingNumber,
		Get: func(r providers.Routing) string {
			return strconv.FormatFloat(r.Weights["price"], 'g', -1, 64)
		},
		IsSet: func(r providers.Routing) bool { return r.Weights != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Weights = nil
				return nil
			}
			v, err := strconv.ParseFloat(input, 64)
			if err != nil {
				return fmt.Errorf("weights.price: %w", err)
			}
			if math.IsNaN(v) || v < 0 || v > 1 {
				return fmt.Errorf("weights.price: %v out of [0,1]", v)
			}
			r.Weights = map[string]float64{"price": v, "headroom": 1 - v}
			return nil
		},
	}
}

func servedAwareField() RoutingField {
	return boolField("servedAware",
		providers.Routing.ServedAwareOn, func(r *providers.Routing) **bool { return &r.ServedAware })
}

// boolField is a tri-state setting: on resolves the default, an empty input clears the override slot.
func boolField(key string, on func(providers.Routing) bool, slot func(*providers.Routing) **bool) RoutingField {
	return RoutingField{
		Key:   key,
		Kind:  RoutingBool,
		Get:   func(r providers.Routing) string { return strconv.FormatBool(on(r)) },
		IsSet: func(r providers.Routing) bool { return *slot(&r) != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				*slot(r) = nil
				return nil
			}
			b, err := strconv.ParseBool(input)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*slot(r) = &b
			return nil
		},
	}
}

// opencodeModule is opencode's own sub-pick knobs: the paid-backend ladder, the work-repo markers that
// block zen-free, which aliases zen-free may serve, and the free dispatch's stall deadline.
type opencodeModule struct{}

func (opencodeModule) Name() string { return "opencode" }

func (opencodeModule) Fields() []RoutingField {
	return []RoutingField{ladderField(), workRepoMarkersField(), freeTiersField(), freeFirstOutputField()}
}

func splitList(input string) []string {
	if strings.TrimSpace(input) == "" {
		return []string{}
	}
	return textutil.SplitCSV(input)
}

func listField(key string, get func(providers.Routing) string, slot func(*providers.Routing) **[]string) RoutingField {
	return RoutingField{
		Key:   key,
		Kind:  RoutingList,
		Get:   get,
		IsSet: func(r providers.Routing) bool { return *slot(&r) != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				*slot(r) = nil
				return nil
			}
			l := splitList(input)
			*slot(r) = &l
			return nil
		},
	}
}

func ladderField() RoutingField {
	return listField("opencode.ladder",
		func(r providers.Routing) string { return strings.Join(r.Opencode.LadderOrder(), ",") },
		func(r *providers.Routing) **[]string { return &r.Opencode.Ladder })
}

func workRepoMarkersField() RoutingField {
	return listField("opencode.workRepoMarkers",
		func(r providers.Routing) string {
			m := r.Opencode.Markers()
			if len(m) == 0 {
				return "none (free models allowed in every repo)"
			}
			return strings.Join(m, ",")
		},
		func(r *providers.Routing) **[]string { return &r.Opencode.WorkRepoMarkers })
}

var freeTierValues = []string{"sonnet,haiku", "sonnet", "haiku", "none"}

func freeTiersField() RoutingField {
	return RoutingField{
		Key:    "opencode.freeTiers",
		Kind:   RoutingEnum,
		Values: freeTierValues,
		Get: func(r providers.Routing) string {
			sonnet, haiku := r.Opencode.FreeTierOK("sonnet"), r.Opencode.FreeTierOK("haiku")
			switch {
			case sonnet && haiku:
				return "sonnet,haiku"
			case sonnet:
				return "sonnet"
			case haiku:
				return "haiku"
			default:
				return "none"
			}
		},
		IsSet: func(r providers.Routing) bool { return r.Opencode.FreeTiers != nil },
		Set: func(r *providers.Routing, input string) error {
			switch {
			case input == "":
				r.Opencode.FreeTiers = nil
			case input == "none":
				l := []string{}
				r.Opencode.FreeTiers = &l
			case slices.Contains(freeTierValues, input):
				l := splitList(input)
				r.Opencode.FreeTiers = &l
			default:
				return fmt.Errorf("opencode.freeTiers: %q, want one of %s", input, strings.Join(freeTierValues, " | "))
			}
			return nil
		},
	}
}

func freeFirstOutputField() RoutingField {
	return RoutingField{
		Key:   "opencode.freeFirstOutput",
		Kind:  RoutingText,
		Get:   func(r providers.Routing) string { return r.Opencode.FirstOutput().String() },
		IsSet: func(r providers.Routing) bool { return r.Opencode.FreeFirstOutput != "" },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Opencode.FreeFirstOutput = ""
				return nil
			}
			d, err := time.ParseDuration(input)
			if err != nil || d <= 0 {
				return fmt.Errorf("opencode.freeFirstOutput: %q must be a positive Go duration", input)
			}
			r.Opencode.FreeFirstOutput = input
			return nil
		},
	}
}
