package catalog

import (
	"path"
	"strings"
	"testing"
)

// The shipped ranks parse, every fact is ordinal 1..5, every pattern is valid and the tiers climb.
func TestRanksData(t *testing.T) {
	r := loadRanks()
	if f, s, d := r.Tiers["fast"], r.Tiers["standard"], r.Tiers["deep"]; f < 1 || f >= s || s >= d {
		t.Errorf("tiers = %v, want fast < standard < deep, all set", r.Tiers)
	}
	for _, f := range r.Families {
		if _, err := path.Match(f.Match, "x"); err != nil {
			t.Errorf("%s: %v", f.Match, err)
		}
		for _, v := range []int{f.Capability, f.Speed, f.Cost} {
			if v < 1 || v > 5 {
				t.Errorf("%s: %+v has a fact outside 1..5", f.Match, f)
			}
		}
	}
}

// A pattern every id of which an earlier pattern already matches never ranks anything (first match wins).
func TestRanksNoShadowedPattern(t *testing.T) {
	fam := loadRanks().Families
	for j, b := range fam {
		text := strings.ReplaceAll(b.Match, "*", "\x00") // only a star of the earlier pattern can consume b's star
		for _, a := range fam[:j] {
			if ok, _ := path.Match(a.Match, text); ok {
				t.Errorf("%s is shadowed by the earlier %s", b.Match, a.Match)
			}
		}
	}
}

// A lite id of a family that also has a plain flash rank keeps the lite rank, not the plain flash one.
func TestRankOfLiteBeforeFlash(t *testing.T) {
	for id, want := range map[string]int{"gemini-3-8-flash-lite": 1, "gemini-3-8-flash": 3, "gemini-3-5-flash-lite": 1} {
		if got := RankOf(id).Capability; got != want {
			t.Errorf("RankOf(%s).Capability = %d, want %d", id, got, want)
		}
	}
}

// Hydrogen's agy ids carry an effort suffix; every one ranks as the family, between lite and Argon.
func TestRankOfHydrogenEffortIDs(t *testing.T) {
	for _, id := range []string{"gemini-4-hydrogen", "gemini-4-hydrogen-medium", "gemini-4-hydrogen-xhigh"} {
		if r := RankOf(id); !r.Known || r.Capability != 3 || r.Match != "gemini-4-hydrogen*" {
			t.Errorf("RankOf(%s) = %+v, want the gemini-4-hydrogen* row at capability 3", id, r)
		}
	}
}

// A dated or prefixed id inherits its family's rank; an id no family matches is unknown.
func TestRankOfFamily(t *testing.T) {
	base := RankOf("claude-opus-5-5")
	for _, id := range []string{"claude-opus-5-5-20261201", "databricks-claude-opus-5-5"} {
		if got := RankOf(id); !got.Known || got.Match != base.Match {
			t.Errorf("RankOf(%s) = %+v, want %s's family", id, got, base.Match)
		}
	}
	if RankOf("zz-unranked-1").Known {
		t.Error("an unmatched id is known")
	}
}

// stubRanks swaps the table for one whose facts the rules alone decide.
func stubRanks(t *testing.T) {
	t.Helper()
	old := loadRanks
	t.Cleanup(func() { loadRanks = old })
	tbl := rankTable{Tiers: map[string]int{"fast": 1, "standard": 3, "deep": 5}, Families: []Rank{
		{Match: "big*", Capability: 5, Speed: 1, Cost: 5},
		{Match: "mid-a*", Capability: 3, Speed: 3, Cost: 3},
		{Match: "mid-b*", Capability: 3, Speed: 3, Cost: 2},
		{Match: "quick*", Capability: 1, Speed: 5, Cost: 2},
		{Match: "tiny*", Capability: 1, Speed: 4, Cost: 1},
		{Match: "twin*", Capability: 3, Speed: 3, Cost: 3},
	}}
	for i := range tbl.Families {
		tbl.Families[i].Known, tbl.Families[i].order = true, i
	}
	loadRanks = func() rankTable { return tbl }
}

func cands(ids ...string) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		out[i] = Candidate{Model: id}
	}
	return out
}

// Choose: at or above the tier, cheapest then fastest; fast is fastest then cheapest; ties by data order then name.
func TestChoose(t *testing.T) {
	stubRanks(t)
	noTools := Candidate{Model: "mid-b", Tools: "no"}
	for _, tc := range []struct {
		name, tier string
		in         []Candidate
		tools      bool
		want       string
		stepped    bool
	}{
		{"standard prefers cost", "standard", cands("big", "mid-a", "mid-b", "quick"), false, "mid-b", false},
		{"deep only at or above", "deep", cands("mid-a", "big", "mid-b"), false, "big", false},
		{"fast prefers speed", "fast", cands("tiny", "quick", "mid-b"), false, "quick", false},
		{"tie by data order", "standard", cands("twin", "mid-a"), false, "mid-a", false},
		{"tie by name", "standard", cands("mid-a-y", "mid-a-x"), false, "mid-a-x", false},
		{"step down", "deep", cands("quick", "mid-a"), false, "mid-a", true},
		{"tools drop a refusing probe", "standard", []Candidate{noTools, {Model: "mid-a"}}, true, "mid-a", false},
		{"no tools keeps it", "standard", []Candidate{noTools, {Model: "mid-a"}}, false, "mid-b", false},
		{"unknown never deep", "deep", cands("zz"), false, "", false},
		{"unknown never standard", "standard", cands("zz"), false, "", false},
		{"unknown fast after known", "fast", cands("zz", "tiny"), false, "tiny", false},
		{"unknown fast needs a tool probe", "fast", cands("zz"), true, "", false},
		{"unknown fast with a tool probe", "fast", []Candidate{{Model: "zz", Tools: "yes"}}, true, "zz", false},
	} {
		c := Choose(tc.in, tc.tier, tc.tools)
		got := ""
		if len(c.Ranked) > 0 {
			got = c.Ranked[0].Model
		}
		if got != tc.want || c.Stepped != tc.stepped {
			t.Errorf("%s: picked %q stepped %v, want %q stepped %v", tc.name, got, c.Stepped, tc.want, tc.stepped)
		}
	}
}

// The literal-prefix fast path of RankOf agrees with path.Match on every family, slash tails included.
func TestRankOfMatchesPathMatch(t *testing.T) {
	tab := loadRanks()
	for _, f := range tab.Families {
		stem := strings.ReplaceAll(strings.ReplaceAll(f.Match, "*", "x"), "?", "y")
		for _, id := range []string{stem, stem + "-1", stem + "/a", "x/" + stem, strings.TrimSuffix(stem, "x")} {
			var want Rank
			for _, r := range tab.Families {
				if ok, _ := path.Match(r.Match, ID(id)); ok {
					want = r
					break
				}
			}
			if got := RankOf(id); got.Match != want.Match {
				t.Errorf("RankOf(%q).Match = %q, want %q", id, got.Match, want.Match)
			}
		}
	}
}

func BenchmarkRankOf(b *testing.B) {
	ids := []string{"claude-opus-5-5-20261001", "databricks-gpt-5-5", "gemini-3-8-flash-lite", "no-such-model"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		RankOf(ids[i%len(ids)])
	}
}

func BenchmarkChoose(b *testing.B) {
	cands := []Candidate{
		{"claude-opus-5-5", "yes"}, {"claude-sonnet-5-5", "yes"}, {"claude-haiku-5-5", ""},
		{"databricks-gpt-5-5", "no"}, {"gemini-3-8-flash", "yes"}, {"no-such-model", "yes"},
	}
	b.ReportAllocs()
	for b.Loop() {
		Choose(cands, "standard", true)
	}
}
