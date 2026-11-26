package agent

import (
	"cmp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
)

// FitOutcomes keeps successes and quality failures; records from the oldest live dispatch on wait, so none is skipped.
func FitOutcomes(recs []Record) []catalog.Outcome {
	var hold time.Time
	for _, r := range recs {
		if r.Status == "running" && r.Alive() && (hold.IsZero() || r.Time.Before(hold)) {
			hold = r.Time
		}
	}
	var out []catalog.Outcome
	for _, r := range recs {
		if o, ok := fitOutcome(r); ok && (hold.IsZero() || o.Time.Before(hold)) {
			out = append(out, o)
		}
	}
	return out
}

func fitOutcome(r Record) (catalog.Outcome, bool) {
	ok := r.Status == "ok"
	if !ok && (!r.Failed() || r.FailKind != FailQuality) {
		return catalog.Outcome{}, false
	}
	alias, _ := TaskTier(r.Request.Prompt)
	match := ""
	for _, m := range []string{r.Invoked, r.Model} {
		if _, bare, found := strings.Cut(m, ":"); found {
			m = bare
		}
		if rk := catalog.RankOf(m); rk.Known {
			match = rk.Match
			break
		}
	}
	if alias == "" || match == "" {
		return catalog.Outcome{}, false
	}
	o := catalog.Outcome{Time: r.Time, Alias: alias, Agent: cmp.Or(r.RanAs, r.Agent), Match: match, OK: ok}
	if ok {
		o.Secs = float64(r.DurationMs) / 1000
	}
	return o, true
}
