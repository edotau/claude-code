package agent

import (
	"cmp"
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
		{name: "typed fence", stage: "last", reg: withRow, o: RunOptions{FenceTyped: true}, p: with(func(p *RoutePlan) { p.Fence = route.Fence{Profiles: []string{"dev"}} }),
			barred: "fence"},
		{name: "tier", stage: "last", reg: withRow, p: with(func(p *RoutePlan) { p.Tier.Asked = models.Opus }), barred: "tier deep"},
		{name: "cap", stage: "last", reg: withRow, p: open, o: RunOptions{Prompt: strings.Repeat("xxxx", testMaxPrompt+1)},
			barred: "max_prompt"},
		{name: "typed provider", stage: "last", reg: withRow, p: open, o: RunOptions{Provider: "databricks:dev"}, barred: "--provider"},
		{name: "line overlay route", stage: "last", reg: withRow, p: open, o: RunOptions{Route: route.Route{Provider: "databricks"}}, barred: "--provider"},
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

func TestPlanLocalInheritedFence(t *testing.T) {
	setLocalStage(t, "last")
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"local": {Name: "local", Provider: "magnitude", Options: providers.Options{LastResort: true}}}}
	fenced := RoutePlan{Agent: "pi", Fence: route.Fence{Profiles: []string{"dev"}}}
	for _, r := range []struct {
		name, barred string
		src          router.PolicySource
	}{{"arm default pin", "", router.PolicyDefault}, {"env pin", "", router.PolicyEnv}, {"typed pin", "keeps the planned", router.PolicyTyped}} {
		p := fenced
		p.Fallback = router.Policy{Kind: router.PolicySameWorkspace, Source: r.src}
		if l := planLocal(context.Background(), reg, RunOptions{}, p); !strings.Contains(l.Barred, r.barred) || (l.Barred == "") != (r.barred == "") {
			t.Errorf("%s: Barred = %q, want %q", r.name, l.Barred, r.barred)
		}
	}
	p := fenced
	p.Local = planLocal(context.Background(), reg, RunOptions{}, p)
	ctx := withPlan(context.Background(), p)
	if got, err := TokenScope(ctx, "local"); err != nil || !slices.Equal(got, []string{"local"}) {
		t.Errorf("TokenScope(local) = %v, %v; want [local]", got, err)
	}
	if _, err := TokenScope(ctx, "qa"); err == nil {
		t.Error("TokenScope(qa) admitted a profile outside the fence")
	}
	if f := localFenced(ctx, p.Fence, []string{"local", "dev"}); f.Allows("local") {
		t.Error("a multi-profile token was widened to the local row")
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
		wait   time.Duration // the slot wait the local run's timeout is debited for
		herr   string        // the hosted hop's error, 429 when empty
		o      RunOptions
		want   bool // the local hop ran
	}{
		{name: "up", o: RunOptions{Prompt: "p", Timeout: time.Minute}, want: true},
		{name: "slot wait debited", wait: 300 * time.Millisecond, o: RunOptions{Prompt: "p", Timeout: time.Minute}, want: true},
		{name: "down", o: RunOptions{Prompt: "p"}, refuse: true},
		{name: "pinned backend", o: RunOptions{Prompt: "p", Provider: "databricks:dev"}},
		{name: "repicks spent", herr: "503 Service Unavailable", o: RunOptions{Prompt: "p", Timeout: time.Minute, Repicks: MaxAutoRepicks}, want: true},
		{name: "wrapped prompt over the cap", o: RunOptions{Prompt: strings.Repeat("xxxx", testMaxPrompt+1)}},
	}
	for _, r := range rows {
		noted = ""
		var lines []string
		var bounded bool
		localGate = func(ctx context.Context, _ func(string)) (func(), error) {
			_, bounded = ctx.Deadline()
			time.Sleep(r.wait)
			if r.refuse {
				return func() {}, errors.New("local model down: connection refused")
			}
			return func() {}, nil
		}
		herr := errors.New(cmp.Or(r.herr, "429 Too Many Requests"))
		hosted := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{herr, herr, herr, herr}}
		local := &fakeRunner{name: "pi", errs: []error{nil}}
		c := chainOf([]Engine{hosted})
		c.Dispatches = append(c.Dispatches, Dispatch{Agent: local, Account: "magnitude·local"})
		c.Plan = &RoutePlan{Agent: "codex", Local: &LocalHop{Workspace: Workspace{Profile: "local", Account: "magnitude·local"}}}
		res, err := runWithFallback(context.Background(), c, r.o, wire.Discard, func(l string) { lines = append(lines, l) })
		announced := slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "fallback ") })
		if r.want != (err == nil) || r.want != (len(local.seen) == 1) || announced != r.want {
			t.Errorf("%s: err=%v local runs=%d announced=%v lines=%q", r.name, err, len(local.seen), announced, lines)
		}
		checkLocalRun(t, r.name, r.want && r.herr == "", r.wait, r.o.Timeout, local.seen, res, noted, bounded)
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

func checkLocalRun(t *testing.T, name string, want bool, wait, timeout time.Duration, seen []RunOptions, res wire.Result, noted string, bounded bool) {
	t.Helper()
	if !want || len(seen) != 1 {
		return
	}
	if wait > 0 && seen[0].Timeout > timeout-wait {
		t.Errorf("%s: local run timeout %s keeps the %s slot wait", name, seen[0].Timeout, wait)
	}
	if res.FallbackKind != "rate limited" || noted != "rate limited" || !bounded {
		t.Errorf("%s: kind=%q noted=%q slot wait bounded by the timeout=%v", name, res.FallbackKind, noted, bounded)
	}
}
