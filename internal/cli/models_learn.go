package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var modelsLearnPage = helptext.Page{
	Usage: "claude-code models learn [flags]",
	About: "Folds the dispatch records into the routing fit table, state/harness/router/fit.json.\n" +
		"A success or a quality failure counts; availability failures say nothing about a model.\n" +
		"Only records newer than the last run are folded, so running it twice changes nothing.",
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
	prev, err := catalog.ReadFit()
	if err != nil {
		return helptext.Fail("models learn: %v", err)
	}
	now := time.Now()
	next := prev.Fold(agent.FitOutcomes(agent.RecentRecords(agent.AllRecords)), now)
	if !*dry {
		if err := catalog.SaveFit(next); err != nil {
			return helptext.Fail("models learn: %v", err)
		}
	}
	return printFit(next, now, *format)
}

func printFit(f catalog.Fit, now time.Time, format string) int {
	views := make([]fitCellView, len(f.Cells))
	for i, c := range f.Cells {
		e := c.Eval(now)
		views[i] = fitCellView{c, e.N, e.Score, e.MeanSecs, !e.Evidence()}
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Created time.Time     `json:"created"`
			Live    bool          `json:"live"`
			Cells   []fitCellView `json:"cells"`
		}{f.Created, (&f).Live(now), views}); err != nil {
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
	if err := stdoutf("models learn", "%s", b.String()); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}
