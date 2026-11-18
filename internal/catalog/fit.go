package catalog

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	fitSchema    = 1
	fitHalfLife  = 14 * 24 * time.Hour
	fitShadow    = 14 * 24 * time.Hour // shadow lasts this long after fit.json is first created
	fitPriorN    = 5.0                 // the prior's weight, and the evidence a cell needs before it may reorder
	fitBand      = 0.1                 // fit lifts within one band tie, so speed may break the tie
	fitNoSpeed   = math.MaxFloat64
	fitCapOK     = 0.9
	fitCapShort1 = 0.6
	fitCapShort2 = 0.3
)

var fitFormat = schemas.Register(schemas.Format{
	Name: "route-fit", Owner: "internal/catalog", Kind: schemas.Record, Version: fitSchema,
	Glob: FitPath,
	New:  func() any { return new(Fit) },
	Keep: "learned from dispatch records that `make sweep` removes after 7 days, so it cannot be rebuilt",
})

// fitNow is the clock Choose and the learn step read; a var so tests need no sleeps.
var fitNow = time.Now

// FitPath is the learned fit table under the harness state dir.
func FitPath() string { return filepath.Join(paths.StateDir(), "router", "fit.json") }

// FitCell is one (alias, agent, group) cell as of At: decayed successes A, quality failures B, timed runs and seconds.
type FitCell struct {
	Alias string    `json:"alias"`
	Agent string    `json:"agent"`
	Match string    `json:"match"` // the ranks.json pattern the model matched
	A     float64   `json:"a"`
	B     float64   `json:"b"`
	Runs  float64   `json:"runs"`
	Secs  float64   `json:"secs"`
	At    time.Time `json:"at"`
}

// Fit is fit.json: Created starts the shadow period; Folded is the newest dispatch record already counted.
type Fit struct {
	Schema  int       `json:"schema"`
	Created time.Time `json:"created"`
	Folded  time.Time `json:"folded"`
	Cells   []FitCell `json:"cells"`
}

// Outcome is one dispatch that says something about its model: a success or a quality failure.
type Outcome struct {
	Time                time.Time
	Alias, Agent, Match string
	OK                  bool
	Secs                float64 // run duration, successes only
}

// FitEval is a cell read at a moment: its Beta mean, the evidence n beyond the prior, and the mean seconds.
type FitEval struct {
	A, B, N, Score, MeanSecs float64
}

func decay(x float64, from, to time.Time) float64 {
	if !to.After(from) {
		return x
	}
	return x * math.Exp2(-float64(to.Sub(from))/float64(fitHalfLife))
}

// Fold counts the outcomes newer than f.Folded into a copy of f, each cell decayed to the outcome's time.
func (f Fit) Fold(outs []Outcome, now time.Time) Fit {
	next := f
	next.Schema, next.Cells = fitSchema, slices.Clone(f.Cells)
	if next.Created.IsZero() {
		next.Created = now
	}
	fresh := slices.DeleteFunc(slices.Clone(outs), func(o Outcome) bool { return !o.Time.After(f.Folded) })
	slices.SortStableFunc(fresh, func(a, b Outcome) int { return a.Time.Compare(b.Time) })
	for _, o := range fresh {
		i := slices.IndexFunc(next.Cells, func(c FitCell) bool { return c.Alias == o.Alias && c.Agent == o.Agent && c.Match == o.Match })
		if i < 0 {
			next.Cells = append(next.Cells, FitCell{Alias: o.Alias, Agent: o.Agent, Match: o.Match, At: o.Time})
			i = len(next.Cells) - 1
		}
		c := &next.Cells[i]
		c.A, c.B, c.Runs, c.Secs = decay(c.A, c.At, o.Time), decay(c.B, c.At, o.Time), decay(c.Runs, c.At, o.Time), decay(c.Secs, c.At, o.Time)
		c.At = o.Time
		switch {
		case !o.OK:
			c.B++
		case o.Secs > 0:
			c.A, c.Runs, c.Secs = c.A+1, c.Runs+1, c.Secs+o.Secs
		default:
			c.A++
		}
		next.Folded = o.Time
	}
	slices.SortFunc(next.Cells, func(a, b FitCell) int {
		return cmp.Or(cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Match, b.Match))
	})
	return next
}

// fitPrior is the success rate a model of capability cap is believed to have on work needing need.
func fitPrior(capability, need int) float64 {
	switch d := capability - need; {
	case d >= 0:
		return fitCapOK
	case d == -1:
		return fitCapShort1
	}
	return fitCapShort2
}

// Eval reads c at now: evidence decays toward the prior, which never does.
func (c FitCell) Eval(now time.Time) FitEval {
	t := loadRanks()
	capability := 0
	for _, r := range t.Families {
		if r.Match == c.Match {
			capability = r.Capability
		}
	}
	return evalCell(c, now, fitPrior(capability, t.Tiers[AliasTier(c.Alias)]))
}

func evalCell(c FitCell, now time.Time, prior float64) FitEval {
	a, b, runs := decay(c.A, c.At, now), decay(c.B, c.At, now), decay(c.Runs, c.At, now)
	e := FitEval{A: a, B: b, N: a + b, Score: (prior*fitPriorN + a) / (fitPriorN + a + b)}
	if runs > 0 {
		e.MeanSecs = decay(c.Secs, c.At, now) / runs
	}
	return e
}

// Evidence is true once a cell holds as much evidence as the prior weighs; below it the cell is its prior.
func (e FitEval) Evidence() bool { return e.N >= fitPriorN }

// Live is true once the table has run in shadow for fitShadow.
func (f *Fit) Live(now time.Time) bool { return !f.Created.IsZero() && now.Sub(f.Created) > fitShadow }

func (f *Fit) cell(alias, agent, match string) (FitCell, bool) {
	if f == nil {
		return FitCell{}, false
	}
	i := slices.IndexFunc(f.Cells, func(c FitCell) bool { return c.Alias == alias && c.Agent == agent && c.Match == match })
	if i < 0 {
		return FitCell{}, false
	}
	return f.Cells[i], true
}

// AliasTier is the dispatch tier word of a model alias, "" for an unknown alias.
func AliasTier(alias string) string {
	for tier, a := range tierAlias {
		if a == alias {
			return tier
		}
	}
	return ""
}

var tierAlias = map[string]string{"fast": models.Haiku, "standard": models.Sonnet, "deep": models.Opus}

var fitCache paths.StampCache[*Fit]

// LoadFit is the learned table, or nil when there is none or it cannot be read (a pick never fails on it).
func LoadFit() *Fit {
	f, _ := fitCache.Get(FitPath(), readFit)
	return f
}

func readFit() (*Fit, error) {
	raw, err := os.ReadFile(FitPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := new(Fit)
	if _, err := schemas.Decode(fitFormat, raw, f); err != nil {
		return nil, fmt.Errorf("fit.json: %w", err)
	}
	return f, nil
}

// ReadFit is LoadFit for the learn step: a missing file is an empty table, a broken one an error.
func ReadFit() (Fit, error) {
	f, err := readFit()
	if f == nil {
		return Fit{}, err
	}
	return *f, nil
}

// SaveFit writes f atomically.
func SaveFit(f Fit) error {
	if err := os.MkdirAll(filepath.Dir(FitPath()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(FitPath(), append(data, '\n'), 0o600)
}

// attach sets r's fit score from c and, once live and c has evidence, the keys pickOrder reorders by.
func (r *Ranked) attach(c FitCell, need int, now time.Time, live bool) {
	prior := fitPrior(r.Capability, need)
	e := evalCell(c, now, prior)
	r.Fit, r.Secs, r.shadow = e.Score, e.MeanSecs, !live
	if live && e.Evidence() {
		r.lift, r.speed = int(math.Round((e.Score-prior)/fitBand)), cmp.Or(e.MeanSecs, fitNoSpeed)
	}
}

// FitNote is the label clause for a ranked pick: the score, the mean seconds, and (shadow) that it changed nothing.
func (r Ranked) FitNote() string {
	if r.Fit == 0 {
		return ""
	}
	s := fmt.Sprintf("fit %.2f", r.Fit)
	if r.Secs > 0 {
		s += fmt.Sprintf(" avg %.0fs", r.Secs)
	}
	if r.shadow {
		s += " (shadow)"
	}
	return s
}
