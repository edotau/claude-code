package providers

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPriceUSD(t *testing.T) {
	p := Price{Input: 3, Output: 4, CacheRead: 0.5, CacheWrite: 1, CacheWrite1h: 2}
	if got := p.USD(1_000_000, 0, 0, 0, 0); got != 3 {
		t.Errorf("input = %v, want 3", got)
	}
	if got := p.USD(0, 0, 0, 1_000_000, 400_000); got != 1.4 {
		t.Errorf("1h write = %v, want 1.4", got)
	}
	if got := (Price{CacheWrite: 1}).USD(0, 0, 0, 1_000_000, 400_000); got != 1 {
		t.Errorf("fallback write = %v, want 1", got)
	}
	if got := p.USD(0, 1_000_000, 1_000_000, 1_000_000, 2_000_000); got != 6.5 {
		t.Errorf("clamped write plus output and read = %v, want 6.5", got)
	}
}

func TestOwnerPrice(t *testing.T) {
	p := &Provider{Options: Options{Prices: map[string]Price{"databricks-claude-opus-5-5[1m]": {Input: 3}}}}
	for _, model := range []string{"claude-opus-5-5", "databricks-claude-opus-5-5[1m]"} {
		if got, ok := p.OwnerPrice(model); !ok || got.Input != 3 {
			t.Errorf("OwnerPrice(%q) = %+v, %v; want input 3", model, got, ok)
		}
	}
	if _, ok := p.OwnerPrice("unknown"); ok {
		t.Error("an unknown model must not be priced")
	}
}

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
		{"control char", Options{Prices: map[string]Price{"m\x1b[31m": ok}}, "printable"},
		{"too long", Options{Prices: map[string]Price{strings.Repeat("m", 201): ok}}, "printable"},
		{"case collision", Options{Prices: map[string]Price{"Model-A": ok, "model-a": ok}}, "same model"},
		{"1m collision", Options{Prices: map[string]Price{"model-a": ok, "databricks-model-a[1m]": ok}}, "same model"},
		{"rate too high", Options{Prices: map[string]Price{"m": {Input: 1_000_001}}}, "up to"},
		{"1h rate", Options{Prices: map[string]Price{"m": {CacheWrite1h: 6}}}, ""},
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

func TestPriceNegativeZeroNormalizes(t *testing.T) {
	var p Price
	if err := json.Unmarshal([]byte(`{"input":-0,"output":1}`), &p); err != nil || math.Signbit(p.Input) {
		t.Errorf("Price = %+v, err %v; want +0 input", p, err)
	}
}

func TestCostOptionsNeedDatabricksPreset(t *testing.T) {
	p := &Provider{Name: "plain", Options: Options{Prices: map[string]Price{"m": {Input: 1}}}}
	if errs := validatePreset("plain", p); len(errs) != 1 || !strings.Contains(errs[0], "databricks profiles only") {
		t.Errorf("errs = %v", errs)
	}
	p.Options = Options{WarehouseID: "wh1"}
	if errs := validatePreset("plain", p); len(errs) != 1 {
		t.Errorf("warehouse errs = %v", errs)
	}
}
