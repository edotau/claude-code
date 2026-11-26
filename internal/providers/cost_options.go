package providers

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proxy/translate"
)

const (
	maxPriceKeyRunes = 200
	maxRate          = 1_000_000 // USD per million tokens, so a sum cannot overflow to +Inf
)

// Price holds owner-maintained USD rates per million tokens; cache_write_1h falls back to cache_write when 0.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
}

// USD prices token counts; the 1h share of cacheWrite uses CacheWrite1h, else CacheWrite.
func (p Price) USD(input, output, cacheRead, cacheWrite, cacheWrite1h int) float64 {
	w1h := min(cacheWrite1h, cacheWrite)
	rate1h := cmp.Or(p.CacheWrite1h, p.CacheWrite)
	return (float64(input)*p.Input + float64(output)*p.Output + float64(cacheRead)*p.CacheRead +
		float64(cacheWrite-w1h)*p.CacheWrite + float64(w1h)*rate1h) / 1e6
}

// OwnerPrice is the options.prices row naming model in any spelling.
func (p *Provider) OwnerPrice(model string) (Price, bool) {
	key := models.ModelKey(model)
	for id, pr := range p.Options.Prices {
		if models.ModelKey(id) == key {
			return pr, true
		}
	}
	return Price{}, false
}

var warehouseIDRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`) })

// ValidWarehouseID reports whether id is a well-formed SQL warehouse id.
func ValidWarehouseID(id string) bool { return warehouseIDRE().MatchString(id) }

// UnmarshalJSON rejects null rates, which encoding/json otherwise silently turns into zero.
func (p *Price) UnmarshalJSON(data []byte) error {
	type plain Price
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("options.prices: price must be an object")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("options.prices: rates must be numbers")
		}
	}
	var value plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("options.prices: %w", err)
	}
	for _, rate := range []*float64{&value.Input, &value.Output, &value.CacheRead, &value.CacheWrite, &value.CacheWrite1h} {
		if *rate == 0 {
			*rate = 0 // -0 becomes 0
		}
	}
	*p = Price(value)
	return nil
}

func (p Price) valid() bool {
	for _, rate := range []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CacheWrite1h} {
		if rate < 0 || rate > maxRate || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return false
		}
	}
	return true
}

func (o Options) Validate() error {
	if o.Wire != "" && o.Wire != WireServing {
		return fmt.Errorf("options.wire %q is not supported; only %s (/serving-endpoints) is routed", o.Wire, WireServing)
	}
	if o.WarehouseID != "" && !warehouseIDRE().MatchString(o.WarehouseID) {
		return errors.New("options.warehouse_id must be 1-64 alphanumeric characters")
	}
	if _, err := translate.ParseGoogleTools(o.GoogleTools); err != nil {
		return fmt.Errorf("options.google_tools: %w", err)
	}
	return o.validatePrices()
}

func (o Options) validatePrices() error {
	seen := map[string]string{}
	for id, p := range o.Prices {
		if err := checkPriceKey(id); err != nil {
			return err
		}
		if !p.valid() {
			return fmt.Errorf("options.prices: rates must be finite non-negative numbers up to %d USD per million tokens", maxRate)
		}
		key := models.ModelKey(id)
		if other, dup := seen[key]; dup {
			return fmt.Errorf("options.prices: %q and %q name the same model", other, id)
		}
		seen[key] = id
	}
	return nil
}

func checkPriceKey(id string) error {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || utf8.RuneCountInString(id) > maxPriceKeyRunes ||
		strings.IndexFunc(id, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 || models.ModelKey(id) == "" {
		return fmt.Errorf("options.prices: model id must be 1-%d printable characters without surrounding spaces", maxPriceKeyRunes)
	}
	return nil
}
