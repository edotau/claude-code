package usage

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/ui/table"
)

const (
	costEst    = "est."
	costBilled = "billed"
	costWidth  = 80
)

// costPrice mirrors providers.Price (USD per million tokens); swap for it once Options.Prices lands (task 1.6).
type costPrice struct {
	Input, Output, CacheRead, CacheWrite float64
}

// costRow is one profile+model line of the cost report; USD is NaN when the model has no price.
type costRow struct {
	Profile, Model                       string
	Input, Output, CacheRead, CacheWrite int
	DBUs, USD                            float64
	Source                               string
	Through                              time.Time
}

// estimateCost prices ledger entries at or after since with each profile's table; 1h cache writes count as CacheWrite.
func estimateCost(entries []tokenledger.Entry, prices map[string]map[string]costPrice, since time.Time) []costRow {
	type key struct{ profile, model string }
	idx := map[key]*costRow{}
	var order []key
	for _, e := range entries {
		if e.Ts.Before(since) {
			continue
		}
		k := key{e.ProfileName(), e.Model}
		r := idx[k]
		if r == nil {
			r = &costRow{Profile: k.profile, Model: k.model, Source: costEst}
			idx[k] = r
			order = append(order, k)
		}
		r.Input += e.Input
		r.Output += e.Output
		r.CacheRead += e.CacheRead
		r.CacheWrite += e.CacheWrite + e.CacheWrite1h
	}
	out := make([]costRow, 0, len(order))
	for _, k := range order {
		r := *idx[k]
		r.USD = math.NaN()
		if p, ok := prices[k.profile][k.model]; ok {
			r.USD = (float64(r.Input)*p.Input + float64(r.Output)*p.Output +
				float64(r.CacheRead)*p.CacheRead + float64(r.CacheWrite)*p.CacheWrite) / 1e6
		}
		out = append(out, r)
	}
	slices.SortFunc(out, compareCostRows)
	return out
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
	row := costRow{Profile: r[pos["profile"]], Model: r[pos["model"]], Source: costBilled}
	var err error
	if row.DBUs, err = parseCostFloat(r[pos["dbus"]], "dbus"); err != nil {
		return row, err
	}
	if row.USD, err = parseCostFloat(r[pos["usd"]], "usd"); err != nil {
		return row, err
	}
	d := strings.TrimSpace(r[pos["through"]])
	if d != "" {
		if len(d) > 10 {
			d = d[:10] // a timestamp's date part is all the caption shows
		}
		if row.Through, err = time.Parse("2006-01-02", d); err != nil {
			return row, fmt.Errorf("column through: %q is not a date", d)
		}
	}
	return row, nil
}

func parseCostFloat(s, col string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("column %s: %q is not a number", col, s)
	}
	return f, nil
}

// printCostSection prints one table per source, each profile's models then its total line, and a caption.
func printCostSection(out io.Writer, rows []costRow, days int) {
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
		fmt.Fprintf(out, "\nCOST, last %d days (%s)\n", days, src)
		head, body := costTable(src, sub)
		fmt.Fprint(out, table.Render(head, []int{14, 28}, body, costWidth))
		fmt.Fprintln(out, costCaption(src, sub))
	}
}

func costCaption(src string, rows []costRow) string {
	if src == costEst {
		return "est.: local ledger tokens x your price table, not an invoice"
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
	return "billed: system.billing, through " + d
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
		tot := costRow{Profile: rows[i].Profile, Model: "total", USD: math.NaN()}
		for ; j < len(rows) && rows[j].Profile == rows[i].Profile; j++ {
			body = append(body, costCells(src, rows[j]))
			addCost(&tot, rows[j])
		}
		body = append(body, costCells(src, tot))
		i = j
	}
	return head, rightAlign(head, body, 2, last)
}

// addCost sums t with r; USD stays NaN (shown "-") until some row is priced.
func addCost(t *costRow, r costRow) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheRead += r.CacheRead
	t.CacheWrite += r.CacheWrite
	t.DBUs += r.DBUs
	if !math.IsNaN(r.USD) {
		if math.IsNaN(t.USD) {
			t.USD = 0
		}
		t.USD += r.USD
	}
}

func costCells(src string, r costRow) []string {
	usd := "-"
	if !math.IsNaN(r.USD) {
		usd = fmt.Sprintf("$%.2f", r.USD)
	}
	if src == costBilled {
		return []string{dash(r.Profile), dash(r.Model), fmt.Sprintf("%.1f", r.DBUs), usd}
	}
	return []string{dash(r.Profile), dash(r.Model), humanTokens(r.Input), humanTokens(r.Output),
		humanTokens(r.CacheRead), humanTokens(r.CacheWrite), usd}
}
