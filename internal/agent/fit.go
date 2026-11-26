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

// JevLabel is one training row for Open-Jev: a brief and the tier and agent type that finished it well.
type JevLabel struct {
	Brief string `json:"brief"`
	Tier  string `json:"tier"`
	Agent string `json:"agent"`
}

// JevLabels is one training row per successful dispatch: its brief without `Tier:` lines, requested tier and agent.
func JevLabels(recs []Record) []JevLabel {
	var out []JevLabel
	for _, r := range recs {
		brief := tierLineRe().ReplaceAllString(jevBrief(r.Request.Prompt), "")
		if tier := tierName(r.Request.Prompt); r.Status == "ok" && tier != "" && r.Agent != "" && brief != "" {
			out = append(out, JevLabel{Brief: strings.TrimSpace(brief), Tier: tier, Agent: r.Agent})
		}
	}
	return out
}
