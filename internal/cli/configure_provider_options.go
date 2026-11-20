package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func (o *configureOpts) conflictsWithAssignment() bool {
	return o.all || o.list || o.restore || o.revert || o.out != "" || o.agents || o.provider != "" || o.agent != ""
}

func configureProviderOptions(o *configureOpts, pos []string) int {
	if o.conflictsWithAssignment() {
		return helptext.FailUsage("configure provider: option assignments cannot be combined with selection, listing or rendering flags")
	}
	reg, err := loadProfile(pos[0])
	if err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	p := reg.Providers[pos[0]]
	if p.Preset != providers.PresetDatabricks {
		return helptext.FailUsage("configure provider: cost options need a Databricks preset profile")
	}
	opts, err := providerOptionPairs(p.Options, pos[1:])
	if err != nil {
		return helptext.FailUsage("configure provider: %v", err)
	}
	if o.dryRun || o.check {
		return helptext.WriteJSONOrFail("configure provider", opts)
	}
	if err := providers.SetProviderFields(p.Name, map[string]any{"options": opts}); err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	return helptext.WriteJSONOrFail("configure provider", opts)
}

func providerOptionPairs(opts providers.Options, pairs []string) (providers.Options, error) {
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return opts, errors.New("expected options.<key>=<value>")
		}
		switch key {
		case "options.warehouse_id":
			opts.WarehouseID = value
		case "options.prices":
			var prices map[string]providers.Price
			if err := json.Unmarshal([]byte(value), &prices); err != nil {
				return opts, fmt.Errorf("options.prices: %w", err)
			}
			opts.Prices = prices
		default:
			return opts, fmt.Errorf("unknown key %q; use options.warehouse_id or options.prices", key)
		}
	}
	return opts, opts.Validate()
}
