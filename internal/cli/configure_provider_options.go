package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func (o *configureOpts) conflictsWithAssignment() bool {
	return o.all || o.out != "" || o.agent != "" || o.allAgents
}

func configureProviderOptions(o *configureOpts, pos []string) int {
	if o.conflictsWithAssignment() {
		return helptext.FailUsage("configure provider: option assignments cannot be combined with selection or rendering flags")
	}
	reg, err := loadProfile(pos[0])
	if err != nil {
		return helptext.Fail("configure provider: %v", err)
	}
	p := reg.Providers[pos[0]]
	opts, err := checkedProviderOptions(reg, p, pos[1:])
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

// checkedProviderOptions applies pairs to p's options; cost options need a preset, last_resort a fit row.
func checkedProviderOptions(reg *providers.Registry, p *providers.Provider, pairs []string) (providers.Options, error) {
	if p.Preset != providers.PresetDatabricks && !onlyLastResort(pairs) {
		return p.Options, errors.New("cost options need a Databricks preset profile")
	}
	opts, err := providerOptionPairs(p.Options, pairs)
	if err == nil && opts.LastResort {
		err = providers.CheckLastResort(reg, p.Name, *p)
	}
	return opts, err
}

// onlyLastResort is true when every pair (at least one) sets options.last_resort, the one non-preset option.
func onlyLastResort(pairs []string) bool {
	if len(pairs) == 0 {
		return false
	}
	for _, pair := range pairs {
		if !strings.HasPrefix(pair, "options.last_resort=") {
			return false
		}
	}
	return true
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
		case "options.google_tools":
			opts.GoogleTools = textutil.SplitCSV(value)
		case "options.last_resort":
			b, err := strconv.ParseBool(value)
			if err != nil || value != strconv.FormatBool(b) {
				return opts, fmt.Errorf("options.last_resort %q: use true or false", value)
			}
			opts.LastResort = b
		default:
			return opts, fmt.Errorf("unknown key %q; use options.warehouse_id, options.prices, options.google_tools "+
				"or options.last_resort", key)
		}
	}
	return opts, opts.Validate()
}
