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
	// IsSet reports whether the vault overlay's own routing object (never the merged registry) sets this field.
	IsSet func(providers.Routing) bool
}

// module is one agent's routing fields.
type module interface {
	Name() string
	Fields() []RoutingField
}

// Modules lists every registered module in display/merge order, core routing knobs first.
func Modules() []module {
	return []module{routingModule{}, opencodeModule{}}
}

// routingModule is the core module: the composite-score weight and the served-model switch.
type routingModule struct{}

func (routingModule) Name() string { return "routing" }

func (routingModule) Fields() []RoutingField {
	// Latency rescales first; an explicit price then wins in a multi-key patch.
	return []RoutingField{weightsLatencyField(), weightsPriceField(), weightsHeadroomField(), servedAwareField(),
		spendCapField("sessionUSD", func(r *providers.Routing) **float64 { return &r.SpendCap.SessionUSD }),
		spendCapField("dayUSD", func(r *providers.Routing) **float64 { return &r.SpendCap.DayUSD })}
}

func weightsPriceField() RoutingField {
	return RoutingField{
		Key:  "weights.price",
		Kind: RoutingNumber,
		Get: func(r providers.Routing) string {
			if r.Weights == nil {
				r.Weights = providers.DefaultRouting().Weights
			}
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
			l := latencyWeight(*r)
			rest, ok := weightRoom(l, v)
			if !ok {
				return fmt.Errorf("weights.price: %v leaves no room for latency %v", v, l)
			}
			r.Weights = map[string]float64{"price": v, "headroom": rest, "latency": l}
			return nil
		},
	}
}

// weightRoom is what latency l and weight v leave of 1 (never below 0); ok is false past a rounding epsilon.
func weightRoom(l, v float64) (rest float64, ok bool) {
	return max(0, 1-l-v), v <= 1-l+1e-9
}

// latencyWeight respects an explicitly stored weights map, including a missing (zero) latency.
func latencyWeight(r providers.Routing) float64 {
	if r.Weights == nil {
		return providers.DefaultRouting().Weights["latency"]
	}
	return r.Weights["latency"]
}

func weightsHeadroomField() RoutingField {
	return RoutingField{
		Key: "weights.headroom", Kind: RoutingNumber,
		Get: func(r providers.Routing) string {
			if r.Weights == nil {
				r.Weights = providers.DefaultRouting().Weights
			}
			return strconv.FormatFloat(r.Weights["headroom"], 'g', -1, 64)
		},
		IsSet: func(r providers.Routing) bool { return r.Weights != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Weights = nil
				return nil
			}
			h, err := strconv.ParseFloat(input, 64)
			if err != nil || math.IsNaN(h) || h < 0 || h > 1 {
				return fmt.Errorf("weights.headroom: %q must be in [0,1]", input)
			}
			l := latencyWeight(*r)
			rest, ok := weightRoom(l, h)
			if !ok {
				return fmt.Errorf("weights.headroom: %v leaves no room for latency %v", h, l)
			}
			r.Weights = map[string]float64{"price": rest, "headroom": h, "latency": l}
			return nil
		},
	}
}

func weightsLatencyField() RoutingField {
	return RoutingField{
		Key: "weights.latency", Kind: RoutingNumber,
		Get:   func(r providers.Routing) string { return strconv.FormatFloat(latencyWeight(r), 'g', -1, 64) },
		IsSet: func(r providers.Routing) bool { return r.Weights != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				r.Weights = nil
				return nil
			}
			l, err := strconv.ParseFloat(input, 64)
			if err != nil || math.IsNaN(l) || l < 0 || l > 1 {
				return fmt.Errorf("weights.latency: %q must be in [0,1]", input)
			}
			w := r.Weights
			if w == nil {
				w = providers.DefaultRouting().Weights
			}
			p, h := w["price"], w["headroom"]
			if p+h == 0 {
				// At latency=1 there is no ratio left; recover the default ratio.
				w = providers.DefaultRouting().Weights
				p, h = w["price"], w["headroom"]
			}
			price := (1 - l) * p / (p + h)
			r.Weights = map[string]float64{"price": price, "headroom": 1 - l - price, "latency": l}
			return nil
		},
	}
}

func spendCapField(key string, slot func(*providers.Routing) **float64) RoutingField {
	key = "spendCap." + key
	return RoutingField{
		Key: key, Kind: RoutingNumber,
		Get: func(r providers.Routing) string {
			if v := *slot(&r); v != nil {
				return fmt.Sprintf("%.2f", *v)
			}
			return "off"
		},
		IsSet: func(r providers.Routing) bool { return *slot(&r) != nil },
		Set: func(r *providers.Routing, input string) error {
			if input == "" {
				*slot(r) = nil
				return nil
			}
			v, err := strconv.ParseFloat(input, 64)
			if err != nil || !(v > 0 && v <= 1000000) {
				return fmt.Errorf("%s: %q must be a finite USD amount in (0, 1000000]", key, input)
			}
			*slot(r) = &v
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

// opencodeModule is opencode's own sub-pick knobs.
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
