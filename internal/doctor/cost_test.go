package doctor

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestCostReportCheck(t *testing.T) {
	row := func(o providers.Options) *providers.Provider {
		return &providers.Provider{Provider: providers.GroupDatabricks, Options: o}
	}
	prices := map[string]providers.Price{"m1": {Input: 1}, "m2": {Output: 2}}
	tests := []struct {
		name   string
		reg    *providers.Registry
		wantOK bool
		want   []string
	}{
		{"none", &providers.Registry{}, true, []string{"no Databricks profiles"}},
		{"unset", &providers.Registry{Providers: map[string]*providers.Provider{"dev": row(providers.Options{})}}, true,
			[]string{"not set up", "configure provider <profile> options.warehouse_id=<id>", "options.prices="}},
		{"partly", &providers.Registry{Providers: map[string]*providers.Provider{"dev": row(providers.Options{WarehouseID: "wh1"})}}, false,
			[]string{"dev warehouse=set prices=0 models", "options.prices="}},
		{"full", &providers.Registry{Providers: map[string]*providers.Provider{"dev": row(providers.Options{WarehouseID: "wh1", Prices: prices})}}, true,
			[]string{"dev warehouse=set prices=2 models"}},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			got := costReportCheck(c.reg)
			if got.OK != c.wantOK || got.Hard {
				t.Errorf("OK=%v Hard=%v, want OK=%v soft", got.OK, got.Hard, c.wantOK)
			}
			for _, w := range c.want {
				if !strings.Contains(got.Detail, w) {
					t.Errorf("detail %q lacks %q", got.Detail, w)
				}
			}
			if strings.Contains(got.Detail, "wh1") {
				t.Errorf("detail leaks the warehouse id: %q", got.Detail)
			}
		})
	}
}
