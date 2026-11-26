package usage

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/cli/ui/table"
	"github.com/edotau/claude-code/internal/dev/tokenledger"
	"github.com/edotau/claude-code/internal/routing/catalog"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
)

const (
	costEst       = "est."
	costWidth     = 80
	budgetPercent = 100
	budgetWarning = 80
)

// costRow is one profile+model line of the cost report; USD is NaN when unknown (no price).
type costRow struct {
	Profile, Provider, Model                       string
	Input, Output, CacheRead, CacheWrite, CacheW1h int
	USD                                            float64
	Unpriced                                       int // models in a total line whose USD is unknown
	Source                                         string
	List                                           bool // priced from the models.dev list price, not an owner's options.prices
}

// listPrices loads the models.dev list prices once and returns a lookup; a var so tests stay hermetic.
var listPrices = func() listLookup {
	if testing.Testing() {
		return func(string, string) (providers.Price, bool) { return providers.Price{}, false }
	}
	t, _ := catalog.ReadPrices()
	return func(provider, id string) (providers.Price, bool) { return t.ListPrice(provider, id) }
}

// listLookup is the models.dev list price for a serving provider and a ledger model key.
type listLookup func(provider, id string) (providers.Price, bool)

// label is the source shown in JSON: "est. list" for a list-priced row.
func (r costRow) label() string {
	if r.List {
		return r.Source + " list"
	}
	return r.Source
}

// priced is r with its USD from the owner's table, else the list price; unknown stays NaN.
func (r costRow) priced(owner map[string]providers.Price, list listLookup) costRow {
	r.USD = math.NaN()
	if p, ok := owner[r.Model]; ok {
		r.USD = p.USD(r.Input, r.Output, r.CacheRead, r.CacheWrite, r.CacheW1h)
	} else if p, ok := list(r.Provider, r.Model); ok {
		r.USD, r.List = p.USD(r.Input, r.Output, r.CacheRead, r.CacheWrite, r.CacheW1h), true
	}
	return r
}

// ledgerModelKey is the price-table lookup key for a ledger model id: spelling-free, and without a "<profile>:" prefix.
func ledgerModelKey(profile, model string) string {
	m := strings.TrimSpace(model)
	if len(m) > len(profile) && strings.EqualFold(m[:len(profile)+1], profile+":") {
		m = m[len(profile)+1:]
	}
	return models.ModelKey(m)
}

// estimateCost prices ledger entries at or after since; profiles without a table are left out unless none has one.
func estimateCost(entries []tokenledger.Entry, prices map[string]map[string]providers.Price, since time.Time) []costRow {
	type key struct{ profile, provider, model string }
	keyed := map[string]map[string]providers.Price{}
	for profile, table := range prices {
		keyed[profile] = map[string]providers.Price{}
		for id, p := range table {
			keyed[profile][models.ModelKey(id)] = p
		}
	}
	list := listPrices()
	idx := map[key]*costRow{}
	var order []key
	for _, e := range entries {
		if e.Ts.Before(since) || (len(prices) > 0 && keyed[e.ProfileName()] == nil) {
			continue
		}
		k := key{e.ProfileName(), e.Provider, ledgerModelKey(e.ProfileName(), e.Model)}
		r := idx[k]
		if r == nil {
			r = &costRow{Profile: k.profile, Provider: k.provider, Model: k.model, Source: costEst}
			idx[k] = r
			order = append(order, k)
		}
		r.Input += e.Input
		r.Output += e.Output
		r.CacheRead += e.CacheRead
		r.CacheWrite += e.CacheWrite // already includes the 1h part (tokenledger.Entry)
		r.CacheW1h += e.CacheWrite1h
	}
	out := make([]costRow, 0, len(order))
	for _, k := range order {
		out = append(out, idx[k].priced(keyed[k.profile], list))
	}
	slices.SortFunc(out, compareCostRows)
	return out
}

// cacheHit is cache reads over all input (ledger input excludes cache reads); suspect: reads with no uncached input.
func cacheHit(r costRow) (pct int, ok, suspect bool) {
	all := r.CacheRead + r.CacheWrite + r.Input
	if all <= 0 {
		return 0, false, false
	}
	return int(math.Round(float64(r.CacheRead) * budgetPercent / float64(all))), true, r.Input == 0 && r.CacheRead > 0
}

func cacheHitCell(r costRow) string {
	pct, ok, suspect := cacheHit(r)
	switch {
	case !ok:
		return "-"
	case suspect:
		return fmt.Sprintf("%d%%?", pct)
	}
	return fmt.Sprintf("%d%%", pct)
}

func compareCostRows(a, b costRow) int {
	if c := strings.Compare(a.Profile, b.Profile); c != 0 {
		return c
	}
	return strings.Compare(a.Model, b.Model)
}

// printCostSection prints the estimate table, each profile's models then its total line, and a caption.
func printCostSection(out io.Writer, rows []costRow, scope string) {
	if len(rows) == 0 {
		return
	}
	sub := slices.Clone(rows)
	slices.SortStableFunc(sub, compareCostRows)
	fmt.Fprintf(out, "\nCOST, %s (%s)\n", scope, costEst)
	head, body := costTable(sub)
	fmt.Fprint(out, table.Render(head, []int{14, 28}, body, costWidth))
	fmt.Fprintln(out, costCaption(sub))
}

// Budget is one workspace budget's current spend, read by the caller so usage never imports databricks.
type Budget struct {
	Profile, Scope, Period string
	Number                 int
	Spend, Limit           float64
}

func costBudgetLine(b Budget) string {
	scope := "scope unspecified"
	switch b.Scope {
	case "ALERT_CONFIGURATION_SCOPE_TYPE_PER_USER":
		scope = "per-user"
	case "ALERT_CONFIGURATION_SCOPE_TYPE_WORKSPACE":
		scope = "workspace"
	}
	period := "period unspecified"
	switch b.Period {
	case "DAY", "WEEK", "MONTH", "YEAR":
		period = strings.ToLower(b.Period)
	}
	percent := b.Spend / b.Limit * budgetPercent
	warning := ""
	if percent >= budgetWarning {
		warning = " WARNING"
	}
	return fmt.Sprintf("budget est.: %s #%d %s/%s: $%.2f spend / $%.2f limit, %.1f%%%s",
		b.Profile, b.Number, scope, period, b.Spend, b.Limit, percent, warning)
}

func costCaption(rows []costRow) string {
	partial := ""
	if slices.ContainsFunc(rows, func(r costRow) bool { return math.IsNaN(r.USD) }) {
		partial = "; totals leave out unpriced models"
	}
	if slices.ContainsFunc(rows, func(r costRow) bool { return r.List }) {
		partial += "; some prices are models.dev list prices (est. list)"
	}
	return "est.: local ledger tokens x your price table, not an invoice" + partial
}

func costTable(rows []costRow) ([]string, [][]string) {
	head := []string{"PROFILE", "MODEL", "INPUT", "OUTPUT", "CACHE-R", "CACHE-W", "HIT", "USD"}
	var body [][]string
	for i := 0; i < len(rows); {
		j := i
		tot := costRow{Profile: rows[i].Profile, Model: "total", USD: math.NaN()}
		for ; j < len(rows) && rows[j].Profile == rows[i].Profile; j++ {
			body = append(body, costCells(rows[j]))
			addCost(&tot, rows[j])
		}
		if tot.Unpriced > 0 {
			tot.Model = fmt.Sprintf("total (%d unpriced)", tot.Unpriced)
		}
		body = append(body, costCells(tot))
		i = j
	}
	return head, rightAlign(head, body, 2, 7)
}

// addCost sums t with r; USD stays NaN (shown "-") until some row is priced, and Unpriced counts the rest.
func addCost(t *costRow, r costRow) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheRead += r.CacheRead
	t.CacheWrite += r.CacheWrite
	t.USD = addKnown(t.USD, r.USD)
	if math.IsNaN(r.USD) {
		t.Unpriced++
	}
}

// addKnown adds b to a, treating NaN as unknown: it never poisons a sum.
func addKnown(a, b float64) float64 {
	switch {
	case math.IsNaN(b):
		return a
	case math.IsNaN(a):
		return b
	}
	return a + b
}

func costCells(r costRow) []string {
	usd := "-"
	if !math.IsNaN(r.USD) {
		usd = fmt.Sprintf("$%.2f", r.USD)
	}
	return []string{dash(r.Profile), dash(r.Model), humanTokens(r.Input), humanTokens(r.Output),
		humanTokens(r.CacheRead), humanTokens(r.CacheWrite), cacheHitCell(r), usd}
}
