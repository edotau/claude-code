package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	pricesSchema = 1
	// SnapshotURL is the models.dev catalog OpenCode reads; one plain GET, no identifying detail.
	SnapshotURL     = "https://models.dev/api.json"
	snapshotLimit   = 32 << 20 // the real file is about 7 MB
	snapshotTimeout = 30 * time.Second
	pricesFresh     = 12 * time.Hour
	maxSnapRate     = 1_000_000 // USD per million tokens
	maxSnapTokens   = 1 << 30
	maxSnapIDRunes  = 200
)

var pricesFormat = schemas.Register(schemas.Format{
	Name: "model-prices", Owner: "internal/catalog", Kind: schemas.Cache, Version: pricesSchema,
	Glob: PricesPath,
	New:  func() any { return new(Prices) },
	Keep: "rebuilt by `models learn` from the models.dev snapshot",
})

// PricesPath is the learned list-price table under the harness state dir.
func PricesPath() string { return filepath.Join(paths.StateDir(), "router", "prices.json") }

// TierPrice is the rate list that applies once a prompt passes Over tokens.
type TierPrice struct {
	Over       int     `json:"over"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// ModelPrice is a snapshot row in USD per million tokens; Priced is false when an owner price or none covers it.
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

// Prices is prices.json: rows keyed by models.ModelKey, stamped with the fetch time.
type Prices struct {
	Schema  int                   `json:"schema"`
	Fetched time.Time             `json:"fetched"`
	Models  map[string]ModelPrice `json:"models"`
}

// PriceReport counts what one snapshot fold did; Rejected names each refused row and why.
type PriceReport struct {
	Models     int      `json:"models"`
	Priced     int      `json:"priced"`
	Limits     int      `json:"limits"`
	Deprecated int      `json:"deprecated"`
	OwnerKept  int      `json:"owner_kept"` // owner-set prices the snapshot left alone
	Rejected   []string `json:"rejected,omitempty"`
}

// Deprecated reports whether the snapshot marks id deprecated; the model stays usable.
func (p Prices) Deprecated(id string) bool {
	return p.Models[models.ModelKey(id)].Status == "deprecated"
}

// Fresh reports whether the table was fetched within the refresh window of now.
func (p Prices) Fresh(now time.Time) bool {
	return !p.Fetched.IsZero() && !paths.Stale(p.Fetched, pricesFresh, 0, now)
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

// FetchSnapshot GETs the snapshot with the default transport (proxy settings apply); a var so tests stay offline.
var FetchSnapshot = func(ctx context.Context) ([]byte, error) {
	if testing.Testing() {
		return nil, errors.New("network off under go test")
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	status, body, _, err := httpx.Do(ctx, httpx.Call{URL: SnapshotURL, Limit: snapshotLimit,
		Header: http.Header{"Accept": {"application/json"}, "User-Agent": {"model-catalog-client"}}})
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	return body, err
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
	out.Tiers, err = contextTiers(m.Cost.Tiers)
	return out, err
}

func contextTiers(in []snapTier) ([]TierPrice, error) {
	var out []TierPrice
	for _, t := range in {
		if t.Tier.Type != "context" {
			continue
		}
		v, listed, ok := t.vals()
		if t.Tier.Size <= 0 || !tokensOK(t.Tier.Size) || !ok || !listed {
			return nil, errors.New("tier incomplete, negative or absurd")
		}
		out = append(out, TierPrice{t.Tier.Size, v[0], v[1], v[2], v[3]})
	}
	return out, nil
}

// snapFirst are the providers whose row wins when several list one model key; the rest follow by name.
var snapFirst = []string{"anthropic", "openai", "google"}

func snapOrder(provs []string) []string {
	slices.Sort(provs)
	first := slices.DeleteFunc(slices.Clone(snapFirst), func(n string) bool { return !slices.Contains(provs, n) })
	return append(first, slices.DeleteFunc(provs, func(n string) bool { return slices.Contains(snapFirst, n) })...)
}

type snapFold struct {
	out   Prices
	rep   PriceReport
	owner map[string]bool
	// ownerHit are the owner keys whose snapshot price was left out
	ownerHit map[string]bool
}

// FoldSnapshot decodes raw; an owner key loses its snapshot price; a bad row is rejected; prev rows persist.
func FoldSnapshot(raw []byte, ownerPriced map[string]bool, prev Prices, now time.Time) (Prices, PriceReport, error) {
	var provs map[string]struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &provs); err != nil {
		return Prices{}, PriceReport{}, fmt.Errorf("not a models.dev snapshot: %w", err)
	}
	if len(provs) == 0 {
		return Prices{}, PriceReport{}, errors.New("not a models.dev snapshot: no providers")
	}
	f := snapFold{out: Prices{Schema: pricesSchema, Fetched: now, Models: map[string]ModelPrice{}}, owner: ownerPriced,
		ownerHit: map[string]bool{}}
	for _, name := range snapOrder(slices.Collect(maps.Keys(provs))) {
		for _, id := range slices.Sorted(maps.Keys(provs[name].Models)) {
			f.add(name+"/"+id, id, provs[name].Models[id])
		}
	}
	if len(f.out.Models) == 0 {
		return Prices{}, f.rep, errors.New("not a models.dev snapshot: no usable model rows")
	}
	f.finish(prev)
	return f.out, f.rep, nil
}

func snapIDOK(id string) bool {
	return id != "" && id == strings.TrimSpace(id) && utf8.RuneCountInString(id) <= maxSnapIDRunes &&
		strings.IndexFunc(id, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

func (f *snapFold) reject(label, why string) { f.rep.Rejected = append(f.rep.Rejected, label+": "+why) }

func (f *snapFold) add(label, id string, raw json.RawMessage) {
	var m *snapModel
	if !snapIDOK(id) {
		f.reject(label, "bad model id")
		return
	}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		f.reject(label, "unreadable row")
		return
	}
	row, err := m.row()
	if err != nil {
		f.reject(label, err.Error())
		return
	}
	key := models.ModelKey(id)
	if key == "" {
		return
	}
	if f.owner[key] && row.Priced {
		row.Priced, row.Input, row.Output, row.CacheRead, row.CacheWrite, row.Tiers = false, 0, 0, 0, 0, nil
		f.ownerHit[key] = true
	}
	if cur, seen := f.out.Models[key]; seen {
		row = fillGaps(cur, row)
	}
	f.out.Models[key] = row
}

// fillGaps is a with the price and limits it lacks taken from b; a's status stays.
func fillGaps(a, b ModelPrice) ModelPrice {
	if !a.Priced && b.Priced {
		a.Priced, a.Input, a.Output, a.CacheRead, a.CacheWrite, a.Tiers = true, b.Input, b.Output, b.CacheRead, b.CacheWrite, b.Tiers
	}
	a.Context, a.MaxOutput = cmp.Or(a.Context, b.Context), cmp.Or(a.MaxOutput, b.MaxOutput)
	return a
}

func (f *snapFold) finish(prev Prices) {
	for k, v := range prev.Models {
		if _, ok := f.out.Models[k]; !ok {
			f.out.Models[k] = v
		}
	}
	f.rep.Models, f.rep.OwnerKept = len(f.out.Models), len(f.ownerHit)
	for _, row := range f.out.Models {
		f.rep.Priced += btoi(row.Priced)
		f.rep.Limits += btoi(row.Context > 0 || row.MaxOutput > 0)
		f.rep.Deprecated += btoi(row.Status == "deprecated")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
