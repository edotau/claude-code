package catalog

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"sync"
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
	order      int
}

type rankTable struct {
	Tiers    map[string]int `json:"tiers"` // tier -> the least capability it needs
	Families []Rank         `json:"families"`
}

// loadRanks parses the embedded ranks once; a malformed embed is a build defect, so it panics (tests cover it).
var loadRanks = sync.OnceValue(func() rankTable {
	var t rankTable
	if err := json.Unmarshal(ranksJSON, &t); err != nil {
		panic("catalog: ranks.json: " + err.Error())
	}
	for i := range t.Families {
		t.Families[i].Known, t.Families[i].order = true, i
	}
	return t
})

// RankOf is id's family rank: the first pattern matching its canonical id; an unknown id sorts after every family.
func RankOf(id string) Rank {
	t := loadRanks()
	canonical := ID(id)
	for _, r := range t.Families {
		if ok, _ := path.Match(r.Match, canonical); ok {
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

// Ranked is a candidate with its rank.
type Ranked struct {
	Model string `json:"model"`
	Rank
}

// Choice is the eligible candidates in pick order; Need is the tier's capability, Stepped is true below it.
type Choice struct {
	Tier    string   `json:"tier"`
	Need    int      `json:"need"`
	Stepped bool     `json:"stepped,omitempty"`
	Ranked  []Ranked `json:"ranked"`
}

// Choose ranks cands for tier: those at or above its capability, else the strongest below; tools drops "no" probes.
func Choose(cands []Candidate, tier string, tools bool) Choice {
	c := Choice{Tier: tier, Need: loadRanks().Tiers[tier]}
	var above, below []Ranked
	for _, cand := range cands {
		r := RankOf(cand.Model)
		if !eligible(r, cand, tier, tools) {
			continue
		}
		if r.Capability >= c.Need || !r.Known {
			above = append(above, Ranked{cand.Model, r})
		} else {
			below = append(below, Ranked{cand.Model, r})
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

// pickOrder is known before unknown, then fast: speed then cost, else cost then speed; then data order, then name.
func pickOrder(tier string) func(a, b Ranked) int {
	return func(a, b Ranked) int {
		first, second := cmp.Compare(a.Cost, b.Cost), cmp.Compare(b.Speed, a.Speed)
		if tier == "fast" {
			first, second = second, first
		}
		known := cmp.Compare(boolRank(b.Known), boolRank(a.Known))
		return cmp.Or(known, first, second, cmp.Compare(a.order, b.order), strings.Compare(a.Model, b.Model))
	}
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
