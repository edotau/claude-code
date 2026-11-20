package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Price holds owner-maintained USD rates per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

var warehouseIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

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
	*p = Price(value)
	return nil
}

func (p Price) valid() bool {
	for _, rate := range []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite} {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return false
		}
	}
	return true
}

func (o Options) Validate() error {
	if o.Wire != "" && o.Wire != WireServing {
		return fmt.Errorf("options.wire %q is not supported; only %s (/serving-endpoints) is routed", o.Wire, WireServing)
	}
	if o.WarehouseID != "" && !warehouseIDRE.MatchString(o.WarehouseID) {
		return errors.New("options.warehouse_id must be 1-64 alphanumeric characters")
	}
	for id, p := range o.Prices {
		if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "\r\n\t") {
			return errors.New("options.prices: model id must be non-empty without control whitespace")
		}
		if !p.valid() {
			return errors.New("options.prices: rates must be finite non-negative numbers in USD per million tokens")
		}
	}
	return nil
}
