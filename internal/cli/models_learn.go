package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var modelsLearnPage = helptext.Page{
	Usage: "claude-code models learn [flags]",
	About: "Folds the dispatch records into the routing fit table, state/harness/router/fit.json.\n" +
		"A success or a quality failure counts; availability failures say nothing about a model.\n" +
		"Only records newer than the last run are folded, so running it twice changes nothing.\n" +
		"It also fills prices and limits from the models.dev snapshot into router/prices.json (one GET,\n" +
		"at most every 12 hours); owner prices (options.prices) win, and an unreachable snapshot changes nothing.",
	Hint: "--model auto shows the fit for 14 days after the file is created, then\n" +
		"reorders by it. Deleting fit.json restarts that period.",
}

// fitCellView is one fit cell as the learn verb prints it.
type fitCellView struct {
	catalog.FitCell
	N     float64 `json:"n"`
	Score float64 `json:"score"`
	Secs  float64 `json:"mean_secs,omitempty"`
	Prior bool    `json:"prior"`
}

func modelsLearn(args []string) int {
	fs := helptext.NewFlagSet("models learn", modelsLearnPage)
	dry := fs.Bool("dry-run", false, "fold and print, write nothing")
	format := helptext.FormatFlag(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 0 {
		return helptext.FailUsage("models learn: unexpected argument %q", rest[0])
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("models learn", err)
	}
	now := time.Now()
	next, err := foldFit(now, *dry)
	if err != nil {
		return helptext.Fail("models learn: %v", err)
	}
	return printFit(next, foldPrices(now, *dry), now, *format)
}

// priceRetry is the least wait between two snapshot attempts, a failed one included.
const priceRetry = time.Hour

// foldPrices folds the snapshot into prices.json (unless dry); a failed fetch leaves it as it was, with a note.
func foldPrices(now time.Time, dry bool) (rep *catalog.PriceReport) {
	if dry {
		return foldPricesLocked(now, true)
	}
	err := paths.WithFileLock(catalog.PricesPath(), func() error { rep = foldPricesLocked(now, false); return nil })
	if err != nil {
		fmt.Fprintf(os.Stderr, "models learn: prices.json busy (%v); prices.json unchanged\n", err)
	}
	return rep
}

// foldPricesLocked is foldPrices with the table's lock already held (a dry run takes none, it writes nothing).
func foldPricesLocked(now time.Time, dry bool) *catalog.PriceReport {
	prev, err := catalog.ReadPrices()
	switch {
	case errors.Is(err, schemas.ErrNewer):
		fmt.Fprintf(os.Stderr, "models learn: %v; prices.json left as it is\n", err)
		return nil
	case err != nil:
		fmt.Fprintf(os.Stderr, "models learn: %v; refetching the price snapshot\n", err)
	case prev.Fresh(now), !dry && !catalog.ClaimEvery("prices-attempt", priceRetry, now):
		return nil
	}
	next, rep, err := fetchFold(prev, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models learn: price snapshot %v; prices.json unchanged\n", err)
		return nil
	}
	if !dry {
		if err := catalog.SavePrices(next); err != nil {
			fmt.Fprintf(os.Stderr, "models learn: cannot write prices.json: %v\n", err)
			return nil
		}
	}
	for _, r := range rep.Rejected {
		fmt.Fprintf(os.Stderr, "models learn: snapshot row rejected, %s\n", r)
	}
	return &rep
}

// fetchFold fetches the snapshot and folds it over prev; the error says which step failed.
func fetchFold(prev catalog.Prices, now time.Time) (catalog.Prices, catalog.PriceReport, error) {
	raw, err := catalog.FetchSnapshot(context.Background())
	if err != nil {
		return catalog.Prices{}, catalog.PriceReport{}, fmt.Errorf("unavailable (%w)", err)
	}
	owner, err := ownerPriced()
	if err != nil {
		return catalog.Prices{}, catalog.PriceReport{}, fmt.Errorf("skipped, owner prices unreadable (%w)", err)
	}
	next, rep, err := catalog.FoldSnapshot(raw, owner, prev, now)
	if err != nil {
		err = fmt.Errorf("ignored (%w)", err)
	}
	return next, rep, err
}

// ownerPriced is the model keys a Databricks profile prices by hand (options.prices).
func ownerPriced() (map[string]bool, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range providers.DatabricksProfiles(reg) {
		if p := reg.Row(name); p != nil {
			for id := range p.Options.Prices {
				out[models.ModelKey(id)] = true
			}
		}
	}
	return out, nil
}

// foldFit folds the dispatch records into fit.json (unless dry) and returns the new table.
func foldFit(now time.Time, dry bool) (catalog.Fit, error) {
	prev, err := catalog.ReadFit()
	if err != nil {
		return catalog.Fit{}, err
	}
	next := prev.Fold(agent.FitOutcomes(agent.RecentRecords(agent.AllRecords)), now)
	if dry {
		return next, nil
	}
	return next, catalog.SaveFit(next)
}

func printFit(f catalog.Fit, prices *catalog.PriceReport, now time.Time, format string) int {
	views := make([]fitCellView, len(f.Cells))
	for i, c := range f.Cells {
		e := c.Eval(now)
		views[i] = fitCellView{c, e.N, e.Score, e.MeanSecs, !e.Evidence()}
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Created time.Time            `json:"created"`
			Live    bool                 `json:"live"`
			Cells   []fitCellView        `json:"cells"`
			Prices  *catalog.PriceReport `json:"prices,omitempty"`
		}{f.Created, (&f).Live(now), views, prices}); err != nil {
			return helptext.Fail("models learn: cannot write JSON: %v", err)
		}
		return 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fit table created %s, %s\n", f.Created.Local().Format("2006-01-02"), map[bool]string{true: "live", false: "shadow"}[(&f).Live(now)])
	fmt.Fprintf(&b, "%-9s %-9s %-24s %6s %6s %6s %6s %8s\n", "ALIAS", "AGENT", "GROUP", "A", "B", "N", "SCORE", "AVG-SECS")
	for _, v := range views {
		note := ""
		if v.Prior {
			note = "  prior"
		}
		fmt.Fprintf(&b, "%-9s %-9s %-24s %6.2f %6.2f %6.2f %6.2f %8.0f%s\n", v.Alias, v.Agent, v.Match, v.A, v.B, v.N, v.Score, v.Secs, note)
	}
	if prices != nil {
		fmt.Fprintf(&b, "price snapshot: %d models, %d priced, %d with limits, %d deprecated, %d owner prices kept, %d rows rejected\n",
			prices.Models, prices.Priced, prices.Limits, prices.Deprecated, prices.OwnerKept, len(prices.Rejected))
	}
	if err := stdoutf("models learn", "%s", b.String()); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}
