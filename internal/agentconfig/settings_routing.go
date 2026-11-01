package agentconfig

import (
	"fmt"
	"strconv"

	"github.com/edotau/claude-code/internal/providers"
)

// routingModule is the core module: the composite-score weight and the subagent offload switch.
type routingModule struct{}

func (routingModule) Name() string { return "routing" }

func (routingModule) Fields() []Field {
	return []Field{weightsPriceField(), subagentsField(), servedAwareField()}
}

// Env exports HARNESS_ROUTE_SUBAGENTS=0 when subagent offload is off; on (the default) exports nothing,
// since the hook path's own default is already on.
func (routingModule) Env(r providers.Routing) map[string]string {
	if r.SubagentsOn() {
		return nil
	}
	return map[string]string{providers.EnvRouteSubagents: "0"}
}

func weightsPriceField() Field {
	return Field{
		Key:   "weights.price",
		Label: "price weight",
		Help:  "0..1; headroom gets the rest, so the two weights still sum to 1",
		Kind:  KindNumber,
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
			if v < 0 || v > 1 {
				return fmt.Errorf("weights.price: %v out of [0,1]", v)
			}
			r.Weights = map[string]float64{"price": v, "headroom": 1 - v}
			return nil
		},
	}
}

func subagentsField() Field {
	return boolField("subagents", "subagents", "offload pressure to subagents; off pins HARNESS_ROUTE_SUBAGENTS=0 at launch",
		providers.Routing.SubagentsOn, func(r *providers.Routing) **bool { return &r.Subagents })
}

func servedAwareField() Field {
	return boolField("servedAware", "served-aware", "pick Databricks models the workspace lists READY; off = static menu",
		providers.Routing.ServedAwareOn, func(r *providers.Routing) **bool { return &r.ServedAware })
}

// boolField is a tri-state setting: on resolves the default, an empty input clears the override slot.
func boolField(key, label, help string, on func(providers.Routing) bool, slot func(*providers.Routing) **bool) Field {
	return Field{
		Key:   key,
		Label: label,
		Help:  help,
		Kind:  KindBool,
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
