package catalog

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed ranks.json
var ranksJSON []byte

// Rank is a model's ordinal facts from ranks.json, each higher-is-more; Known is false when no pattern matched.
type Rank struct {
	Match      string `json:"match"`
	Capability int    `json:"capability"`
	Speed      int    `json:"speed"`
	Cost       int    `json:"cost"`
	Known      bool   `json:"known"`
	Rotate     *bool  `json:"rotate,omitempty"` // false keeps the row out of auto rotation; a typed id still runs
	order      int
	prefix     string // Match's literal stem when it is "stem*", else ""; set at load
}

type rotationRule struct {
	Min     int `json:"min"`      // least capability a rotation member needs
	MaxCost int `json:"max_cost"` // dearest cost a rotation member may have
	// a model at cost >= DeferCost takes one of every DeferTurns of its turns; both 0 = no deferral
	DeferCost  int `json:"defer_cost,omitempty"`
	DeferTurns int `json:"defer_turns,omitempty"`
}

type rankTable struct {
	Tiers    map[string]int          `json:"tiers"` // tier -> the least capability it needs
	Rotation map[string]rotationRule `json:"rotation"`
	Families []Rank                  `json:"families"`
}

// loadRanks parses the embedded ranks once; a malformed embed is a build defect, so it panics (tests cover it).
var loadRanks = sync.OnceValue(func() rankTable {
	var t rankTable
	if err := json.Unmarshal(ranksJSON, &t); err != nil {
		panic("catalog: ranks.json: " + err.Error())
	}
	for i := range t.Families {
		f := &t.Families[i]
		f.Known, f.order = true, i
		if stem, ok := strings.CutSuffix(f.Match, "*"); ok && !strings.ContainsAny(stem, `*?[\`) {
			f.prefix = stem
		}
	}
	return t
})

// RankOf is id's family rank: the first pattern matching its canonical id; an unknown id sorts after every family.
func RankOf(id string) Rank {
	t := loadRanks()
	canonical := strings.ReplaceAll(ID(id), ".", "-") // Copilot spells 5.5 where the rows spell 5-5
	for _, r := range t.Families {
		if r.prefix != "" {
			// path.Match's trailing star spans any run without a slash
			if rest, ok := strings.CutPrefix(canonical, r.prefix); ok && !strings.Contains(rest, "/") {
				return r
			}
		} else if ok, _ := path.Match(r.Match, canonical); ok {
			return r
		}
	}
	return Rank{order: len(t.Families)}
}

// Candidate is one model a route serves, with its probe's tool verdict ("yes", "no" or "" for never probed).
type Candidate struct {
	Model string
	Tools string
}

type Ranked struct {
	Model string  `json:"model"`
	Fit   float64 `json:"fit,omitempty"`  // learned success score; 0 when the cell has none
	Secs  float64 `json:"secs,omitempty"` // mean seconds of its successful runs
	Rank
	shadow bool
	lift   int     // set only once the fit is live and the cell has evidence
	speed  float64 // mean seconds, fitNoSpeed when unknown; set with lift
}

// Choice is the eligible candidates in pick order; Need is the tier's capability, Stepped is true below it.
type Choice struct {
	Tier    string   `json:"tier"`
	Need    int      `json:"need"`
	Stepped bool     `json:"stepped,omitempty"`
	Shadow  bool     `json:"fit_shadow,omitempty"` // the fit scores are shown, not applied
	Ranked  []Ranked `json:"ranked"`
}

// Choose ranks cands for agent on tier: those at or above its capability, else the strongest below; tools drops "no".
func Choose(cands []Candidate, agent, tier string, tools bool) Choice {
	c := Choice{Tier: tier, Need: loadRanks().Tiers[tier]}
	fit, alias, now, live := LoadFit(), tierAlias[tier], time.Time{}, false
	if fit != nil {
		now = fitNow()
		live = fit.Live(now)
		c.Shadow = !live
	}
	var above, below []Ranked
	for _, cand := range cands {
		r := RankOf(cand.Model)
		if !eligible(r, cand, tier, tools) {
			continue
		}
		ranked := Ranked{Model: cand.Model, Rank: r}
		if cell, ok := fit.cell(alias, agent, r.Match); ok && r.Known {
			ranked.attach(cell, c.Need, now, live)
		}
		if r.Capability >= c.Need || !r.Known {
			above = append(above, ranked)
		} else {
			below = append(below, ranked)
		}
	}
	c.Ranked = above
	if len(above) == 0 && len(below) > 0 {
		top := slices.MaxFunc(below, func(a, b Ranked) int { return a.Capability - b.Capability }).Capability
		c.Ranked, c.Stepped = slices.DeleteFunc(below, func(r Ranked) bool { return r.Capability != top }), true
	}
	slices.SortStableFunc(c.Ranked, pickOrder(tier))
	return c
}

// eligible drops a tool-refusing probe on a tools run; an unknown model runs only the fast tier, tools proven.
func eligible(r Rank, cand Candidate, tier string, tools bool) bool {
	if tools && cand.Tools == "no" {
		return false
	}
	return r.Known || tier == "fast" && (!tools || cand.Tools == "yes")
}

// RotationPool is cands a rotation may run at tier, in input order: eligible, rotating, at the floor, under the cap.
func RotationPool(cands []Candidate, tier string, tools bool) []string {
	rule, ok := loadRanks().Rotation[tier]
	if !ok {
		return nil
	}
	var out []string
	for _, c := range cands {
		r := RankOf(c.Model)
		if !eligible(r, c, tier, tools) || r.Rotate != nil && !*r.Rotate ||
			r.Known && (r.Capability < rule.Min || r.Cost > rule.MaxCost) {
			continue
		}
		out = append(out, c.Model)
	}
	return out
}

// DeferTurns maps each dear pool model (cost >= the tier's defer_cost) to its defer_turns; nil when the tier has none.
func DeferTurns(pool []string, tier string) map[string]int {
	rule := loadRanks().Rotation[tier]
	if rule.DeferTurns <= 0 {
		return nil
	}
	out := map[string]int{}
	for _, m := range pool {
		if r := RankOf(m); r.Known && r.Cost >= rule.DeferCost {
			out[m] = rule.DeferTurns
		}
	}
	return out
}

// pickOrder: known first, then (live fit only) fit lift, then the faster cell within a lift band; then as before.
func pickOrder(tier string) func(a, b Ranked) int {
	return func(a, b Ranked) int {
		first, second := cmp.Compare(a.Cost, b.Cost), cmp.Compare(b.Speed, a.Speed)
		if tier == "fast" {
			first, second = second, first
		}
		known := cmp.Compare(boolRank(b.Known), boolRank(a.Known))
		lift, speed := cmp.Compare(b.lift, a.lift), cmp.Compare(cmp.Or(a.speed, fitNoSpeed), cmp.Or(b.speed, fitNoSpeed))
		return cmp.Or(known, lift, speed, first, second, cmp.Compare(a.order, b.order), strings.Compare(a.Model, b.Model))
	}
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
