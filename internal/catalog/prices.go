package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	pricesSchema   = 1
	snapshotLimit  = 32 << 20  // the real file is about 7 MB
	maxSnapRate    = 1_000_000 // USD per million tokens
	maxSnapTokens  = 1 << 30
	maxSnapIDRunes = 200
)

var pricesFormat = schemas.Register(schemas.Format{
	Name: "model-prices", Owner: "internal/catalog", Kind: schemas.Cache, Version: pricesSchema,
	Glob: PricesPath,
	New:  func() any { return new(Prices) },
	Keep: "rebuilt by `models learn` from OpenCode's models.dev snapshot file",
})

// PricesPath is the learned list-price table under the harness state dir.
func PricesPath() string { return filepath.Join(paths.StateDir(), "router", "prices.json") }

// SnapshotPath is the models.dev file OpenCode keeps: OPENCODE_MODELS_PATH, else <cache home>/opencode/models.json.
func SnapshotPath() string {
	if p := os.Getenv("OPENCODE_MODELS_PATH"); p != "" {
		return p
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "opencode", "models.json")
}

// TierPrice is the rate list that applies once a prompt passes Over tokens.
type TierPrice struct {
	Over       int     `json:"over"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// ModelPrice is a snapshot row in USD per million tokens; Priced is false when the snapshot lists no price.
type ModelPrice struct {
	Priced     bool        `json:"priced,omitempty"`
	Input      float64     `json:"input,omitempty"`
	Output     float64     `json:"output,omitempty"`
	CacheRead  float64     `json:"cache_read,omitempty"`
	CacheWrite float64     `json:"cache_write,omitempty"`
	Tiers      []TierPrice `json:"tiers,omitempty"`
	Context    int         `json:"context,omitempty"`
	MaxOutput  int         `json:"max_output,omitempty"`
	Status     string      `json:"status,omitempty"`
}

// Prices is prices.json, a mirror of the snapshot as last read: provider -> model id -> row; Read is the file's mtime.
type Prices struct {
	Schema    int                              `json:"schema"`
	Read      time.Time                        `json:"read"`
	Providers map[string]map[string]ModelPrice `json:"providers"`
}

// PriceReport counts what one snapshot read produced; Rejected names each refused row and why.
type PriceReport struct {
	Models     int      `json:"models"`
	Priced     int      `json:"priced"`
	Limits     int      `json:"limits"`
	Deprecated int      `json:"deprecated"`
	OwnerKept  int      `json:"owner_kept"` // snapshot prices an owner-set price outranks
	Rejected   []string `json:"rejected,omitempty"`
}

// Row is provider's row for id.
func (p Prices) Row(provider, id string) (ModelPrice, bool) {
	row, ok := p.Providers[provider][id]
	return row, ok
}

// Lookup is the row for id when exactly one provider lists it; two or none is not known.
func (p Prices) Lookup(id string) (ModelPrice, bool) {
	var found ModelPrice
	n := 0
	for _, rows := range p.Providers {
		if row, ok := rows[id]; ok {
			found, n = row, n+1
		}
	}
	return found, n == 1
}

// vendorOf maps a model id prefix to the snapshot provider that lists the vendor's own list price.
var vendorOf = map[string]string{"claude-": "anthropic", "gpt-": "openai", "gemini-": "google"}

// ListRow is provider's row for id, else the vendor's own row (its id may spell dots as dashes: gpt-6.1-sol).
func (p Prices) ListRow(provider, id string) (ModelPrice, bool) {
	if row, ok := p.Row(provider, provider+"-"+id); ok {
		return row, true
	}
	if row, ok := p.Row(provider, id); ok {
		return row, true
	}
	for prefix, vendor := range vendorOf {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		for _, vid := range slices.Sorted(maps.Keys(p.Providers[vendor])) {
			if strings.ReplaceAll(vid, ".", "-") == strings.ReplaceAll(id, ".", "-") {
				return p.Providers[vendor][vid], true
			}
		}
	}
	return ModelPrice{}, false
}

// Rate is Lookup's row when it carries a price; a reader uses it only where options.prices names none.
func (p Prices) Rate(id string) (ModelPrice, bool) {
	row, ok := p.Lookup(id)
	return row, ok && row.Priced
}

// Deprecated reports whether the snapshot marks id deprecated; the model stays usable.
func (p Prices) Deprecated(id string) bool {
	row, _ := p.Lookup(id)
	return row.Status == "deprecated"
}

// ReadPrices is the table on disk; a missing file is empty, a broken one an error.
func ReadPrices() (Prices, error) {
	raw, err := os.ReadFile(PricesPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Prices{}, nil
	}
	if err != nil {
		return Prices{}, err
	}
	var p Prices
	if _, err := schemas.Decode(pricesFormat, raw, &p); err != nil {
		return Prices{}, fmt.Errorf("prices.json: %w", err)
	}
	return p, nil
}

// SavePrices writes p atomically.
func SavePrices(p Prices) error {
	if err := os.MkdirAll(filepath.Dir(PricesPath()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(PricesPath(), append(data, '\n'), 0o644)
}

// ReadSnapshot reads OpenCode's snapshot file, at most snapshotLimit bytes, with its mtime.
func ReadSnapshot() ([]byte, time.Time, error) {
	f, err := os.Open(SnapshotPath())
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, snapshotLimit+1))
	if err == nil && len(raw) > snapshotLimit {
		err = errors.New("snapshot file too large")
	}
	return raw, fi.ModTime(), err
}

type snapRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// vals is input, output, cache read, cache write (a missing cache rate is 0); listed needs input and output present.
func (r snapRates) vals() (v [4]float64, listed, ok bool) {
	for i, p := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if p == nil {
			continue
		}
		if v[i] = *p; v[i] < 0 || v[i] > maxSnapRate || math.IsNaN(v[i]) {
			return v, false, false
		}
	}
	return v, r.Input != nil && r.Output != nil, true
}

type snapTier struct {
	snapRates
	Tier struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier"`
}

type snapModel struct {
	Cost *struct {
		snapRates
		Tiers []snapTier `json:"tiers"`
	} `json:"cost"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Status string `json:"status"`
}

func tokensOK(n int) bool { return n >= 0 && n <= maxSnapTokens }

// row validates one snapshot model into a ModelPrice; the error says what is wrong, the caller names the row.
func (m snapModel) row() (ModelPrice, error) {
	out := ModelPrice{Context: m.Limit.Context, MaxOutput: m.Limit.Output}
	if !tokensOK(out.Context) || !tokensOK(out.MaxOutput) {
		return ModelPrice{}, errors.New("limit out of range")
	}
	if slices.Contains([]string{"alpha", "beta", "deprecated"}, m.Status) {
		out.Status = m.Status
	}
	if m.Cost == nil {
		return out, nil
	}
	v, listed, ok := m.Cost.vals()
	if !ok {
		return ModelPrice{}, errors.New("price negative or absurd")
	}
	if !listed {
		return out, nil
	}
	out.Priced, out.Input, out.Output, out.CacheRead, out.CacheWrite = true, v[0], v[1], v[2], v[3]
	var err error
	out.Tiers, err = contextTiers(m.Cost.Tiers, v)
	return out, err
}

// contextTiers validates the context tiers; a tier that omits a cache rate inherits the base row's.
func contextTiers(in []snapTier, base [4]float64) ([]TierPrice, error) {
	var out []TierPrice
	for _, t := range in {
		if t.Tier.Type != "context" {
			continue
		}
		v, listed, ok := t.vals()
		if t.Tier.Size <= 0 || !tokensOK(t.Tier.Size) || !ok || !listed {
			return nil, errors.New("tier incomplete, negative or absurd")
		}
		if t.CacheRead == nil {
			v[2] = base[2]
		}
		if t.CacheWrite == nil {
			v[3] = base[3]
		}
		out = append(out, TierPrice{t.Tier.Size, v[0], v[1], v[2], v[3]})
	}
	return out, nil
}

func snapIDOK(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && utf8.RuneCountInString(id) <= maxSnapIDRunes &&
		strings.IndexFunc(id, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// FoldSnapshot decodes raw into a Prices stamped read; a bad row is rejected by name (quoted) and the rest kept.
func FoldSnapshot(raw []byte, read time.Time) (Prices, PriceReport, error) {
	var provs map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &provs); err != nil {
		return Prices{}, PriceReport{}, fmt.Errorf("not a models.dev snapshot: %w", err)
	}
	out, rep := Prices{Schema: pricesSchema, Read: read, Providers: map[string]map[string]ModelPrice{}}, PriceReport{}
	for _, name := range slices.Sorted(maps.Keys(provs)) {
		for _, id := range slices.Sorted(maps.Keys(provs[name].Models)) {
			row, why := foldRow(name, id, provs[name].Models[id])
			if why != "" {
				rep.Rejected = append(rep.Rejected, fmt.Sprintf("%q: %s", name+"/"+id, why))
				continue
			}
			if out.Providers[name] == nil {
				out.Providers[name] = map[string]ModelPrice{}
			}
			out.Providers[name][id] = row
			rep.Models++
			rep.Priced += btoi(row.Priced)
			rep.Limits += btoi(row.Context > 0 || row.MaxOutput > 0)
			rep.Deprecated += btoi(row.Status == "deprecated")
		}
	}
	if rep.Priced+rep.Limits == 0 {
		return Prices{}, rep, errors.New("not a models.dev snapshot: no model row with a price or limit")
	}
	return out, rep, nil
}

// foldRow validates one snapshot row; a non-empty second result is the rejection reason.
func foldRow(provider, id string, raw json.RawMessage) (ModelPrice, string) {
	var m *snapModel
	switch {
	case !snapIDOK(provider) || !snapIDOK(id):
		return ModelPrice{}, "bad model id"
	case json.Unmarshal(raw, &m) != nil || m == nil:
		return ModelPrice{}, "unreadable row"
	}
	row, err := m.row()
	if err != nil {
		return ModelPrice{}, err.Error()
	}
	return row, ""
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
