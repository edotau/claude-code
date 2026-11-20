package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCostOptionsValidate(t *testing.T) {
	ok := Price{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"valid", Options{WarehouseID: "wh1Test", Prices: map[string]Price{"m1": ok, "free": {}}}, ""},
		{"warehouse dash", Options{WarehouseID: "wh-test-1"}, "warehouse_id"},
		{"warehouse space", Options{WarehouseID: "a b"}, "warehouse_id"},
		{"warehouse long", Options{WarehouseID: strings.Repeat("a", 65)}, "warehouse_id"},
		{"negative input", Options{Prices: map[string]Price{"m1": {Input: -1}}}, "non-negative"},
		{"negative cache write", Options{Prices: map[string]Price{"m1": {CacheWrite: -0.1}}}, "non-negative"},
		{"blank model id", Options{Prices: map[string]Price{" ": ok}}, "model id"},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			err := c.opts.Validate()
			if (err == nil) != (c.want == "") || err != nil && !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate() = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestCostOptionsRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.UseKeyring(t)
	opts := Options{WarehouseID: "wh1Test", Prices: map[string]Price{"m1": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}}
	fields := map[string]any{"preset": "databricks", "host": "https://h.example.com", "cliProfile": "w", "options": opts}
	if err := SetProviderFields("dbx", fields); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := reg.Providers["dbx"].Options
	if got.WarehouseID != "wh1Test" || got.Prices["m1"] != opts.Prices["m1"] {
		t.Errorf("round trip = %+v, want %+v", got, opts)
	}
	fields["options"] = Options{WarehouseID: "bad id"}
	if err := SetProviderFields("dbx", fields); err == nil {
		t.Error("overlay accepted an invalid warehouse id")
	}
}

func TestCostOptionsDoNotChangeThePickerScope(t *testing.T) {
	p := &Provider{Name: "dev", Host: "https://example.invalid"}
	before := pickerScope(p)
	p.Options.WarehouseID, p.Options.Prices = "wh1", map[string]Price{"m": {Input: 1}}
	if after := pickerScope(p); after != before {
		t.Errorf("setting cost options changed the picker scope: %s -> %s", before, after)
	}
}
