package usage

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/databricks/sqlexec"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/ui/table"
)

const (
	costEst       = "est."
	costBilled    = "billed"
	costWidth     = 80
	budgetPercent = 100
	budgetWarning = 80
)

// costRow is one profile+model line of the cost report; USD and DBUs are NaN when unknown (no price, SQL NULL).
type costRow struct {
	Profile, Model                                 string
	Input, Output, CacheRead, CacheWrite, CacheW1h int
	DBUs, USD                                      float64
	Unpriced                                       int // models in a total line whose USD is unknown
	Source                                         string
	Through                                        time.Time
}

// ledgerModelKey is the price-table lookup key for a ledger model id: spelling-free, and without a "<profile>:" prefix.
func ledgerModelKey(profile, model string) string {
	m := strings.TrimSpace(model)
	if len(m) > len(profile) && strings.EqualFold(m[:len(profile)+1], profile+":") {
		m = m[len(profile)+1:]
	}
	return models.ModelKey(m)
}

// estimateCost prices ledger entries at or after since with each profile's table; unpriced profiles are left out.
func estimateCost(entries []tokenledger.Entry, prices map[string]map[string]providers.Price, since time.Time) []costRow {
	type key struct{ profile, model string }
	keyed := map[string]map[string]providers.Price{}
	for profile, table := range prices {
		keyed[profile] = map[string]providers.Price{}
		for id, p := range table {
			keyed[profile][models.ModelKey(id)] = p
		}
	}
	idx := map[key]*costRow{}
	var order []key
	for _, e := range entries {
		if e.Ts.Before(since) || keyed[e.ProfileName()] == nil {
			continue
		}
		k := key{e.ProfileName(), ledgerModelKey(e.ProfileName(), e.Model)}
		r := idx[k]
		if r == nil {
			r = &costRow{Profile: k.profile, Model: k.model, Source: costEst}
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
		r := *idx[k]
		r.USD = math.NaN()
		if p, ok := keyed[k.profile][k.model]; ok {
			r.USD = priceRow(r, p)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, compareCostRows)
	return out
}

// priceRow is r's USD at p; the 1h share of CacheWrite uses cache_write_1h, falling back to cache_write.
func priceRow(r costRow, p providers.Price) float64 {
	w1h := min(r.CacheW1h, r.CacheWrite)
	rate1h := p.CacheWrite1h
	if rate1h == 0 {
		rate1h = p.CacheWrite
	}
	return (float64(r.Input)*p.Input + float64(r.Output)*p.Output + float64(r.CacheRead)*p.CacheRead +
		float64(r.CacheWrite-w1h)*p.CacheWrite + float64(w1h)*rate1h) / 1e6
}

func compareCostRows(a, b costRow) int {
	if c := strings.Compare(a.Profile, b.Profile); c != 0 {
		return c
	}
	return strings.Compare(a.Model, b.Model)
}

// billedRows maps a query result to rows by column name, so a reordered SELECT still reads right.
func billedRows(columns []string, rows [][]string) ([]costRow, error) {
	pos := map[string]int{}
	for i, c := range columns {
		pos[strings.ToLower(strings.TrimSpace(c))] = i
	}
	for _, name := range []string{"profile", "model", "dbus", "usd", "through"} {
		if _, ok := pos[name]; !ok {
			return nil, fmt.Errorf("billing result is missing column %q", name)
		}
	}
	out := make([]costRow, 0, len(rows))
	for n, r := range rows {
		if len(r) < len(columns) {
			return nil, fmt.Errorf("billing row %d has %d cells, want %d", n+1, len(r), len(columns))
		}
		row, err := billedRow(r, pos)
		if err != nil {
			return nil, fmt.Errorf("billing row %d: %w", n+1, err)
		}
		out = append(out, row)
	}
	return out, nil
}

func billedRow(r []string, pos map[string]int) (costRow, error) {
	row := costRow{Profile: cellText(r[pos["profile"]]), Model: cellText(r[pos["model"]]), Source: costBilled}
	var err error
	if row.DBUs, err = parseCostFloat(r[pos["dbus"]], "dbus"); err != nil {
		return row, err
	}
	if row.USD, err = parseCostFloat(r[pos["usd"]], "usd"); err != nil {
		return row, err
	}
	row.Through, err = parseThrough(r[pos["through"]])
	return row, err
}

func cellText(s string) string {
	if s == sqlexec.NullCell {
		return ""
	}
	return s
}

// parseThrough reads a date, an RFC 3339 timestamp or "YYYY-MM-DD HH:MM:SS"; an empty or NULL cell is the zero time.
func parseThrough(s string) (time.Time, error) {
	s = strings.TrimSpace(cellText(s))
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Truncate(24 * time.Hour), nil
		}
	}
	return time.Time{}, fmt.Errorf("column through: %q is not a date (want 2006-01-02, RFC 3339 or 2006-01-02 15:04:05)", s)
}

// parseCostFloat reads a number; an empty or NULL cell is unknown (NaN), never zero.
func parseCostFloat(s, col string) (float64, error) {
	s = strings.TrimSpace(cellText(s))
	if s == "" {
		return math.NaN(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("column %s: %q is not a number", col, s)
	}
	return f, nil
}

// printCostSection prints one table per source, each profile's models then its total line, and a caption.
func printCostSection(out io.Writer, rows []costRow, scope string) {
	for _, src := range []string{costEst, costBilled} {
		var sub []costRow
		for _, r := range rows {
			if r.Source == src {
				sub = append(sub, r)
			}
		}
		if len(sub) == 0 {
			continue
		}
		slices.SortStableFunc(sub, compareCostRows)
		fmt.Fprintf(out, "\nCOST, %s (%s)\n", scope, src)
		head, body := costTable(src, sub)
		fmt.Fprint(out, table.Render(head, []int{14, 28}, body, costWidth))
		fmt.Fprintln(out, costCaption(src, sub))
	}
}

func printCostBudgets(out io.Writer) {
	reg, err := loadCostRegistry()
	if err != nil {
		return
	}
	for _, profile := range providers.DatabricksProfiles(reg) {
		budgets, err := databricks.ReadBudgetSpend(context.Background(), reg.Row(profile))
		if err != nil {
			continue
		}
		for _, budget := range budgets {
			fmt.Fprintln(out, costBudgetLine(profile, budget))
		}
	}
}

func costBudgetLine(profile string, b databricks.BudgetSpend) string {
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
		profile, b.Budget, scope, period, b.Spend, b.Limit, percent, warning)
}

func costCaption(src string, rows []costRow) string {
	partial := ""
	if slices.ContainsFunc(rows, func(r costRow) bool { return math.IsNaN(r.USD) }) {
		partial = "; totals leave out unpriced models"
	}
	if src == costEst {
		return "est.: local ledger tokens x your price table, not an invoice" + partial
	}
	var through time.Time
	for _, r := range rows {
		if r.Through.After(through) {
			through = r.Through
		}
	}
	d := "unknown"
	if !through.IsZero() {
		d = through.Format("2006-01-02")
	}
	return "billed: system.billing, through " + d + partial
}

func costTable(src string, rows []costRow) ([]string, [][]string) {
	head := []string{"PROFILE", "MODEL", "DBUS", "USD"}
	last := 3
	if src == costEst {
		head = []string{"PROFILE", "MODEL", "INPUT", "OUTPUT", "CACHE-R", "CACHE-W", "USD"}
		last = 6
	}
	var body [][]string
	for i := 0; i < len(rows); {
		j := i
		tot := costRow{Profile: rows[i].Profile, Model: "total", USD: math.NaN(), DBUs: math.NaN()}
		for ; j < len(rows) && rows[j].Profile == rows[i].Profile; j++ {
			body = append(body, costCells(src, rows[j]))
			addCost(&tot, rows[j])
		}
		if tot.Unpriced > 0 {
			tot.Model = fmt.Sprintf("total (partial: %d unpriced)", tot.Unpriced)
		}
		body = append(body, costCells(src, tot))
		i = j
	}
	return head, rightAlign(head, body, 2, last)
}

// addCost sums t with r; USD stays NaN (shown "-") until some row is priced, and Unpriced counts the rest.
func addCost(t *costRow, r costRow) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheRead += r.CacheRead
	t.CacheWrite += r.CacheWrite
	t.USD, t.DBUs = addKnown(t.USD, r.USD), addKnown(t.DBUs, r.DBUs)
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

func costCells(src string, r costRow) []string {
	usd := "-"
	if !math.IsNaN(r.USD) {
		usd = fmt.Sprintf("$%.2f", r.USD)
	}
	if src == costBilled {
		dbus := "-"
		if !math.IsNaN(r.DBUs) {
			dbus = fmt.Sprintf("%.1f", r.DBUs)
		}
		return []string{dash(r.Profile), dash(r.Model), dbus, usd}
	}
	return []string{dash(r.Profile), dash(r.Model), humanTokens(r.Input), humanTokens(r.Output),
		humanTokens(r.CacheRead), humanTokens(r.CacheWrite), usd}
}
