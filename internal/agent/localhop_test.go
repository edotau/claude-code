package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const testMaxPrompt = 100 // tokens

func setLocalStage(t *testing.T, stage string) {
	t.Helper()
	prev := localRuleOf
	localRuleOf = func() localRules { return localRules{Stage: stage, Tiers: []string{"fast"}, MaxPrompt: testMaxPrompt} }
	t.Cleanup(func() { localRuleOf = prev })
}

func TestPlanLocal(t *testing.T) {
	withRow := &providers.Registry{Providers: map[string]*providers.Provider{
		"dev":   {Name: "dev", Preset: providers.PresetDatabricks},
		"local": {Name: "local", Provider: "magnitude", Options: providers.Options{LastResort: true}},
	}}
	noRow := &providers.Registry{Providers: map[string]*providers.Provider{"dev": {Name: "dev"}}}
	open := RoutePlan{Agent: "claude", Fence: route.Fence{Open: true}, Tier: Tier{Asked: models.Haiku},
		Fallback: router.Policy{Kind: router.PolicyAnyInFence}}
	with := func(f func(*RoutePlan)) RoutePlan { p := open; f(&p); return p }
	rows := []struct {
		name, stage string
		reg         *providers.Registry
		o           RunOptions
		p           RoutePlan
		nilHop      bool
		barred      string // substring; "" = not barred
		hop         bool
	}{
		{name: "no row", stage: "last", reg: noRow, p: open, nilHop: true},
		{name: "none", stage: "last", reg: withRow, p: with(func(p *RoutePlan) { p.Fallback.Kind = router.PolicyNone }), barred: "--fallback none"},
		{name: "typed model", stage: "last", reg: withRow, p: open,
			o: RunOptions{Model: "databricks-claude-sonnet-5-5", ModelTyped: true}, barred: "typed model"},
		{name: "closed fence", stage: "last", reg: withRow, p: with(func(p *RoutePlan) { p.Fence = route.Fence{Profiles: []string{"dev"}} }),
			barred: "fence"},
		{name: "tier", stage: "last", reg: withRow, p: with(func(p *RoutePlan) { p.Tier.Asked = models.Opus }), barred: "tier deep"},
		{name: "cap", stage: "last", reg: withRow, p: open, o: RunOptions{Prompt: strings.Repeat("xxxx", testMaxPrompt+1)},
			barred: "max_prompt"},
		{name: "typed provider", stage: "last", reg: withRow, p: open, o: RunOptions{Provider: "databricks:dev"}, barred: "--provider"},
		{name: "shadow", stage: "shadow", reg: withRow, p: open},
		{name: "last", stage: "last", reg: withRow, p: open, hop: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			setLocalStage(t, r.stage)
			l := planLocal(context.Background(), r.reg, r.o, r.p)
			if (l == nil) != r.nilHop {
				t.Fatalf("planLocal = %+v, want nil %v", l, r.nilHop)
			}
			if l != nil {
				checkLocalHop(t, l, r.barred, r.hop)
			}
		})
	}
}

func checkLocalHop(t *testing.T, l *LocalHop, barred string, wantHop bool) {
	t.Helper()
	if l.Workspace.Account != "magnitude·local" || !strings.Contains(l.Barred, barred) || (barred == "") != (l.Barred == "") {
		t.Errorf("planLocal = %+v, want account magnitude·local, barred %q", l, barred)
	}
	if h, ok := l.hop("claude"); ok != wantHop || ok && (h.Runner.Kind != RunnerPi || h.Workspace.Profile != "local") {
		t.Errorf("hop = %+v %v, want %v (pi on local)", h, ok, wantHop)
	}
	if got := l.carries(Dispatch{Account: "magnitude·local"}); got != wantHop {
		t.Errorf("carries = %v, want %v: only an admitted hop is the chain's local hop", got, wantHop)
	}
}

func TestLocalHopTakesOver(t *testing.T) {
	vaulttest.Use(t)
	fastDefaultBackoff(t)
	setLocalStage(t, "last")
	var noted string
	prevGate, prevNote := localGate, noteLocalFallback
	t.Cleanup(func() { localGate, noteLocalFallback = prevGate, prevNote })
	noteLocalFallback = func(kind, _ string) { noted = kind }
	rows := []struct {
		name   string
		refuse bool
		o      RunOptions
		want   bool // the local hop ran
	}{
		{name: "up", o: RunOptions{Prompt: "p", Timeout: time.Minute}, want: true},
		{name: "down", o: RunOptions{Prompt: "p"}, refuse: true},
		{name: "pinned backend", o: RunOptions{Prompt: "p", Provider: "databricks:dev"}},
		{name: "wrapped prompt over the cap", o: RunOptions{Prompt: strings.Repeat("xxxx", testMaxPrompt+1)}},
	}
	for _, r := range rows {
		noted = ""
		var lines []string
		var bounded bool
		localGate = func(ctx context.Context, _ func(string)) (func(), error) {
			_, bounded = ctx.Deadline()
			if r.refuse {
				return func() {}, errors.New("local model down: connection refused")
			}
			return func() {}, nil
		}
		hosted := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("429 Too Many Requests"), errors.New("429 Too Many Requests"), errors.New("429 Too Many Requests"), errors.New("429 Too Many Requests")}}
		local := &fakeRunner{name: "pi", errs: []error{nil}}
		c := chainOf([]Engine{hosted})
		c.Dispatches = append(c.Dispatches, Dispatch{Agent: local, Account: "magnitude·local"})
		c.Plan = &RoutePlan{Agent: "codex", Local: &LocalHop{Workspace: Workspace{Profile: "local", Account: "magnitude·local"}}}
		res, err := runWithFallback(context.Background(), c, r.o, wire.Discard, func(l string) { lines = append(lines, l) })
		announced := slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "fallback ") })
		if r.want != (err == nil) || r.want != (len(local.seen) == 1) || announced != r.want {
			t.Errorf("%s: err=%v local runs=%d announced=%v lines=%q", r.name, err, len(local.seen), announced, lines)
		}
		if r.want && (res.FallbackKind != "rate limited" || noted != "rate limited" || !bounded) {
			t.Errorf("%s: kind=%q noted=%q slot wait bounded by the timeout=%v", r.name, res.FallbackKind, noted, bounded)
		}
	}
}

func TestSlimForLocal(t *testing.T) {
	o := slimForLocal(Dispatch{}, RunOptions{Prompt: "task" + reviewLoopBlock, ReviewLoop: true, Timeout: time.Hour})
	if o.Prompt != "task" || o.ReviewLoop || o.Effort != "low" || o.Timeout != DefaultToolsTimeout {
		t.Errorf("slimForLocal = %q %v %q %s", o.Prompt, o.ReviewLoop, o.Effort, o.Timeout)
	}
	if o := slimForLocal(Dispatch{Effort: "high"}, RunOptions{Timeout: time.Minute}); o.Effort != "high" || o.Timeout != time.Minute {
		t.Errorf("typed effort and a shorter timeout must stand: %q %s", o.Effort, o.Timeout)
	}
}
