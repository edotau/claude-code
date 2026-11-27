package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths/pathstest"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

// withTestPlan is ctx carrying the plan PlanRoute would hold for o with pool profiles (Databricks rows named dev, ...).
func withTestPlan(t *testing.T, ctx context.Context, o RunOptions, profiles ...string) context.Context {
	t.Helper()
	pol, err := planPolicy(ctx, o, "")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := planFence(ctx, ParentRoute{Fence: route.Fence{Open: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := RoutePlan{Agent: o.Agent, Fence: fence, Fallback: pol}
	for _, p := range profiles {
		plan.Pool = append(plan.Pool, Workspace{Profile: p, Account: "databricks·" + p})
	}
	return withPlan(ctx, plan)
}

// A databricks:<p> confinement plans no pi hop on a sibling workspace and mints no token for one.
func TestPiRunnerConfinedStaysOnWorkspace(t *testing.T) {
	reg, _, _ := autoFixture(t)
	q := RouteReq{Agent: "claude", Model: "databricks-claude-sonnet-5-5", Fallback: route.FallbackAll}
	lead := Dispatch{Account: "databricks·dev", Model: q.Model}
	o := RunOptions{Agent: "claude", Fallback: route.FallbackAll}
	open := withTestPlan(t, proc.WithValues(context.Background(), proc.Values{}), o, "dev", "qa", "prod")
	sibs, skips, _ := piSiblings(open, reg, lead, q.Model)
	if len(sibs)+len(skips) == 0 {
		t.Fatal("fixture: an unconfined ask considers no sibling workspace")
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	armed = withTestPlan(t, armed, o, "dev")
	if sibs, skips, why := piSiblings(armed, reg, lead, q.Model); len(sibs)+len(skips) != 0 || why != "" {
		t.Fatalf("confined to dev, pi hops planned beyond it: hops %v, skips %v, why %q", sibs, skips, why)
	}
	testTokenScope(t, reg, open, armed)
}

// testTokenScope: confined to dev scopes dev only; unconfined keeps asked + extra + fallbacks; a bad overlay refuses.
func testTokenScope(t *testing.T, reg *providers.Registry, open, armed context.Context) {
	t.Helper()
	if _, err := TokenScope(armed, "qa"); err == nil {
		t.Fatal("confined to dev, a token for qa was scoped")
	}
	if got, err := TokenScope(armed, "dev"); err != nil || len(got) != 1 || got[0] != "dev" {
		t.Fatalf("confined token scope = %v, %v; want [dev]", got, err)
	}
	want := slices.Compact(slices.Sorted(slices.Values(append([]string{"dev", "qa"}, reg.Fallback...))))
	if got, err := TokenScope(open, "dev", "qa"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("unconfined token scope = %v, %v; want asked + extra + fallbacks %v", got, err, want)
	}
}

// An empty admission pass leaves existing chain metadata untouched.
func TestPiRunnerNoCandidates(t *testing.T) {
	c := Chain{Want: "claude", Dispatches: []Dispatch{{Model: "m"}}, Preflight: &Preflight{Chosen: "x"}}
	got := newPlannedChain(context.Background(), nil).finish(c)
	if len(got.Dispatches) != 1 || got.Preflight.Chosen != "x" {
		t.Fatalf("empty admission = %+v", got)
	}
}

// The inner line of a --carry runs the carried agent's own CLI; Pi keeps the takeover.
func TestPiRunnerBarredForCarriedChild(t *testing.T) {
	setConfigDir(t)
	now := time.Now()
	record := func(i int, carry bool, status string, pid int) Record {
		r := NewRecord("codex", RunOptions{Carry: carry, Prompt: "the task"}, now.Add(time.Duration(i)*time.Second))
		r.Status, r.PID = status, pid
		r.PIDStart, _ = proc.StartTime(pid)
		if err := r.Save(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	rec := record(0, true, "running", os.Getppid()) // the carrier is this test's parent process
	ctx := proc.WithValues(context.Background(), proc.Values{proc.DispatchIDEnv: rec.ID})
	if why := piBar(ctx, RunOptions{}, "codex", false); !strings.Contains(why, "carried") {
		t.Fatalf("carried codex child: barred = %q, want the carried reason", why)
	}
	if why := piBar(ctx, RunOptions{}, "gemini", false); why != "" {
		t.Fatalf("another agent under a codex carrier is barred: %q", why)
	}
	done, plain := record(1, true, "ok", os.Getppid()), record(2, false, "running", os.Getppid())
	noPID := record(3, true, "running", 0)
	for _, id := range []string{done.ID, plain.ID, noPID.ID, "../" + rec.ID} {
		ctx := proc.WithValues(context.Background(), proc.Values{proc.DispatchIDEnv: id})
		if why := piBar(ctx, RunOptions{}, "codex", false); why != "" {
			t.Fatalf("parent %q is no live carrier, yet barred: %q", id, why)
		}
	}
}

// Every roster agent takes the pi runner on a Databricks lead; an unknown name never does.
func TestPiRunnerAskedForRosterAgents(t *testing.T) {
	reg, _, _ := autoFixture(t)
	plan := RoutePlan{Pool: []Workspace{{Profile: "dev"}}}
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		plan.Agent = name
		if rs, _ := planRunners(context.Background(), reg, RunOptions{}, plan, false); rs[0].Kind != RunnerPi {
			t.Errorf("%s: runners %+v, want pi first", name, rs)
		}
	}
	plan.Agent = "persona"
	if rs, _ := planRunners(context.Background(), reg, RunOptions{}, plan, false); rs[0].Kind == RunnerPi {
		t.Errorf("persona took the pi runner")
	}
}

// A rotation's kept-out rows get no sibling pi hop, hop or skip.
func TestPiSiblingsSkipRotationDrop(t *testing.T) {
	reg, _, _ := autoFixture(t)
	q := RouteReq{Agent: "claude", Model: "databricks-claude-sonnet-5-5", Fallback: route.FallbackAll}
	lead := Dispatch{Account: "databricks·dev", Model: q.Model}
	open := proc.WithValues(context.Background(), proc.Values{})
	sibs, skips, _ := piSiblings(withTestPlan(t, open, RunOptions{Agent: "claude", Fallback: route.FallbackAll}, "dev", "qa", "prod"), reg, lead, q.Model)
	if len(sibs)+len(skips) == 0 {
		t.Fatal("fixture: an unrotated ask considers no sibling workspace")
	}
	var drop []string
	for name := range reg.Providers {
		if name != "dev" {
			drop = append(drop, name)
		}
	}
	ctx := withTestPlan(t, router.WithRotatedLead(open, router.RotatedLead{Row: "dev", Drop: drop}),
		RunOptions{Agent: "claude", Fallback: route.FallbackAll}, "dev") // planPool drops the kept-out rows
	if sibs, skips, why := piSiblings(ctx, reg, lead, q.Model); len(sibs)+len(skips) != 0 || why != "" {
		t.Fatalf("kept-out rows still planned pi hops: hops %v, skips %v, why %q", sibs, skips, why)
	}
}

// A plain pi ask follows the default only when no fallback mode is set anywhere; a typed none, pin or model is one hop.
func TestPiChainModeDefault(t *testing.T) {
	setConfigDir(t)
	ctx := context.Background()
	for _, c := range []struct {
		q    RouteReq
		def  bool
		want string
	}{
		{RouteReq{Agent: "pi", Fallback: route.FallbackNone}, true, route.FallbackAgents},
		{RouteReq{Agent: "pi", Fallback: route.FallbackNone}, false, ""},
		{RouteReq{Agent: "pi", Fallback: route.FallbackNone, Model: "m"}, true, ""},
		{RouteReq{Agent: "pi", Fallback: route.FallbackPin}, false, route.FallbackPin},
		{RouteReq{Agent: "pi", Fallback: route.FallbackNone, Resumed: true}, true, ""},
		{RouteReq{Agent: "pi", Fallback: route.FallbackNone, AutoModel: true}, true, ""},
		{RouteReq{Agent: "pi", Fallback: route.FallbackAgents, Native: true}, false, ""},
		{RouteReq{Agent: "pi", Fallback: route.FallbackAgents}, false, route.FallbackAgents},
	} {
		pctx := withTestPlan(t, ctx, RunOptions{Agent: "pi", Fallback: c.q.Fallback, FallbackDefaulted: c.def})
		if got := piChainMode(pctx, c.q); got != c.want {
			t.Errorf("piChainMode(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
	t.Setenv(router.EnvFallback, route.FallbackNone)
	nctx := withTestPlan(t, ctx, RunOptions{Agent: "pi", Fallback: route.FallbackNone, FallbackDefaulted: true})
	if got := piChainMode(nctx, RouteReq{Agent: "pi", Fallback: route.FallbackNone}); got != "" {
		t.Errorf("an env none gave mode %q, want a one-hop chain", got)
	}
}

// Own CLI candidates keep their order and metadata when no runner plan is present.
func TestPlanCandidateOrderWithoutRunners(t *testing.T) {
	pi, cl := &fakeRunner{name: "pi"}, &fakeRunner{name: "claude"}
	hops := []Dispatch{{Agent: pi, Account: "databricks·workspace-a"}, {Agent: cl, Account: "anthropic·login"}}
	c := planTestChain(context.Background(), RouteReq{}, Chain{Dispatches: hops})
	if len(c.Dispatches) != 2 || c.Dispatches[0].Account != hops[0].Account || c.Dispatches[1].Account != hops[1].Account {
		t.Fatalf("hops = %+v", c.Dispatches)
	}
}

// A plain pi ask gets pi hops on the other workspaces after its lead; a confinement or no hop leaves it alone.
func TestWithPiSiblings(t *testing.T) {
	reg, _, _ := autoFixture(t) // the fixture roster has no pi driver, so each sibling is planned and then skipped
	c := Chain{Dispatches: []Dispatch{{Agent: &fakeRunner{name: "pi"}, Account: "databricks·dev", Model: "databricks-gemini-3-8-flash"}}}
	o := RunOptions{Agent: "pi", Fallback: route.FallbackAgents}
	open := withTestPlan(t, proc.WithValues(context.Background(), proc.Values{}), o, "dev", "qa", "prod")
	got := (&plannedChain{ctx: open, reg: reg, plain: c.Dispatches}).plainSiblings(c, c.Dispatches[0])
	notes := strings.Join(got.Preflight.Notes, "\n")
	for _, ws := range []string{"pi (qa)", "pi (prod)"} {
		if !strings.Contains(notes, ws) {
			t.Fatalf("an open ask never considered %s: %q", ws, notes)
		}
	}
	if strings.Contains(notes, "pi (dev)") {
		t.Fatalf("the lead's own workspace was planned as a sibling: %q", notes)
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	if got := (&plannedChain{ctx: withTestPlan(t, armed, o, "dev"), reg: reg, plain: c.Dispatches}).plainSiblings(c, c.Dispatches[0]); len(got.Dispatches) != 1 || got.Preflight != nil {
		t.Fatalf("a confined ask planned sibling hops: %+v", got)
	}
}

// registerPiForTest adds a pi roster row and driver for one test; the fixture roster lists only the four vendors.
func registerPiForTest(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	piCaps := Caps{Model: Native, Provider: Native, Effort: Native, SessionID: Native, Tools: Native, Autonomous: Native,
		NoPersist: Native, Env: Native, SystemPrompt: Native, AppendSystemPrompt: Native, WorkDir: Emulated} // internal/pi/register.go
	addRow(Schema{Name: "pi", CLI: CLI{Binary: "pi", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI}, Caps: piCaps}})
	drivers["pi"] = fixtureDriver{NewBaseEngine("pi")}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		delete(drivers, "pi")
		rosterRows = slices.DeleteFunc(rosterRows, func(r Schema) bool { return r.Name == "pi" })
	})
}

// A plain pi ask's chain runs pi on dev, pi on the keyed sibling workspace, then claude; a confinement keeps pi on dev.
func TestWithPiClaudeHopOrder(t *testing.T) {
	autoFixture(t)
	keyed := strings.NewReplacer(`"qa":{"auth":{"type":"oauth"}`, `"qa":{"auth":{"type":"key","command":"echo qzvxplk"}`)
	vaulttest.Put(t, vault.Overlay, "providers", keyed.Replace(autoOverlay))
	registerPiForTest(t)
	q := RouteReq{Agent: "pi", Fallback: route.FallbackAgents}
	pi := &fakeRunner{name: "pi"}
	c := Chain{Dispatches: []Dispatch{{Agent: pi, Account: "databricks·dev", Model: "databricks-gemini-3-8-flash"}}}
	open := proc.WithValues(context.Background(), proc.Values{})
	got := planTestChain(withTestPlan(t, open, RunOptions{Agent: "pi", Fallback: route.FallbackAgents}, "dev", "qa", "prod"), q, c)
	var seq []string
	for _, d := range got.Dispatches {
		seq = append(seq, d.Agent.Name()+" "+d.Account)
	}
	want := []string{"pi databricks·dev", "pi databricks·qa", "claude databricks·dev"}
	if !slices.Equal(seq, want) {
		t.Fatalf("hops = %v (notes %+v), want %v", seq, got.Preflight, want)
	}
	if sib := got.Dispatches[1]; sib.Model != "databricks-gemini-3-8-flash" {
		t.Fatalf("the sibling runs %q, not the lead's model", sib.Model)
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	if got := planTestChain(withTestPlan(t, armed, RunOptions{Agent: "pi", Fallback: route.FallbackAgents}, "dev"), q, c); len(got.Dispatches) != 2 || got.Dispatches[1].Agent.Name() != "claude" {
		t.Fatalf("a confined ask's hops = %+v, want pi then claude", got.Dispatches)
	}
}

// Under pin a plain pi ask is pi then claude on the same workspace; an unreadable overlay never widens.
func TestWithPiClaudeHopPin(t *testing.T) {
	autoFixture(t)
	registerPiForTest(t)
	pi := &fakeRunner{name: "pi"}
	c := Chain{Dispatches: []Dispatch{{Agent: pi, Account: "databricks·dev", Model: "databricks-gemini-3-8-flash"}}}
	open := withTestPlan(t, proc.WithValues(context.Background(), proc.Values{}), RunOptions{Agent: "pi", Fallback: route.FallbackPin}, "dev")
	if pinned := piChainMode(open, RouteReq{Agent: "pi", Fallback: route.FallbackPin}); pinned != route.FallbackPin {
		t.Fatalf("plain pin ask mode = %q", pinned)
	}
	var pseq []string
	for _, d := range planTestChain(open, RouteReq{Agent: "pi", Fallback: route.FallbackPin}, c).Dispatches {
		pseq = append(pseq, d.Agent.Name()+" "+d.Account)
	}
	if pwant := []string{"pi databricks·dev", "claude databricks·dev"}; !slices.Equal(pseq, pwant) {
		t.Fatalf("pin hops = %v, want %v", pseq, pwant)
	}
	stray := Chain{Dispatches: []Dispatch{c.Dispatches[0], {Agent: pi, Account: "databricks·qa"}}}
	if got := planTestChain(open, RouteReq{Agent: "pi", Fallback: route.FallbackPin}, stray); len(got.Dispatches) != 2 {
		t.Fatalf("pin kept a stray workspace hop: %+v", got.Dispatches)
	}
	bad := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{bad`})
	if _, err := PlanRoute(bad, RunOptions{Agent: "pi", Fallback: route.FallbackNone, FallbackDefaulted: true}, ParentRoute{}); err == nil {
		t.Fatal("a malformed overlay built a plan")
	}
	one := planTestChain(bad, RouteReq{Agent: "pi", Fallback: route.FallbackNone}, c) // no plan, no widening
	if len(one.Dispatches) != 1 {
		t.Fatalf("a malformed overlay widened: %+v", one)
	}
}

// Any one of the overlay, the process env and the vault setting a fallback mode ends the default.
func TestPlanPolicySources(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	ctx := context.Background()
	o := RunOptions{Agent: "pi", Fallback: route.FallbackNone, FallbackDefaulted: true}
	src := func(ctx context.Context) router.PolicySource {
		pol, err := planPolicy(ctx, o, "")
		if err != nil {
			t.Fatal(err)
		}
		return pol.Source
	}
	if got := src(ctx); got != router.PolicyDefault {
		t.Fatalf("nothing set anywhere = source %d", got)
	}
	if got := src(proc.WithValues(ctx, proc.Values{router.EnvFallback: "agents"})); got != router.PolicyEnv {
		t.Errorf("a process env fallback = source %d", got)
	}
	if pol, err := planPolicy(ctx, o, "agents"); err != nil || pol.Source != router.PolicyOverlay {
		t.Errorf("an overlay fallback = %+v, %v", pol, err)
	}
	vaulttest.Put(t, vault.Env, router.EnvFallback, "pin")
	if got := src(ctx); got != router.PolicyEnv {
		t.Error("a vault fallback reads as unset")
	}
}

// The ask's runner capabilities bar the pi runner before a rotation pick is stamped.
func TestPiRunnerBarredForRunnerCaps(t *testing.T) {
	setConfigDir(t)
	registerPiForTest(t)
	ctx := context.Background()
	budget := 1.0
	for name, o := range map[string]RunOptions{
		"carry": {Carry: true}, "debug file": {DebugFile: "x"}, "fleet": {Fleet: true}, "memory": {EnableMemory: true},
		"budget": {BudgetUSD: &budget},
	} {
		if why := piBar(ctx, o, "codex", false); why == "" {
			t.Errorf("%s: want a reason", name)
		}
	}
	if why := piBar(ctx, RunOptions{}, "codex", false); why != "" {
		t.Errorf("plain ask barred: %q", why)
	}
}

// planTestChain feeds candidate dispatches through the same append-only assembly used by route admission.
func planTestChain(ctx context.Context, q RouteReq, c Chain) Chain {
	assembly := newPlannedChain(context.WithValue(ctx, routeRequestKey{}, q), loadRegistryWarn())
	candidates := c.Dispatches
	c.Dispatches = nil
	for _, d := range candidates {
		c = assembly.append(c, d)
	}
	return assembly.finish(c)
}

// plannedRunnerFixture uses synthetic workspaces with the same served model and usable sibling credentials.
func plannedRunnerFixture(t *testing.T) (*providers.Registry, context.Context) {
	t.Helper()
	setLocalStage(t, "off")
	setConfigDir(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{
 "workspace-a":{"auth":{"type":"key","command":"echo test-token"},"host":"https://workspace-a.example","preset":"databricks"},
 "workspace-b":{"auth":{"type":"key","command":"echo test-token"},"host":"https://workspace-b.example","preset":"databricks"}}}}`)
	for _, profile := range []string{"workspace-a", "workspace-b"} {
		saveListing(t, profile, []string{"databricks-claude-sonnet-5-5"})
	}
	registerPiForTest(t)
	pathstest.FakeVendor(t, "pi", "")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := proc.WithValues(context.Background(), proc.Values{providers.EnvSessionProfile: "workspace-a"})
	return reg, PeekContext(ctx)
}

func TestPiRunnerSinglePassMetadata(t *testing.T) {
	reg, ctx := plannedRunnerFixture(t)
	o := RunOptions{Agent: "claude", Model: "sonnet", Lead: "workspace-a", Fallback: route.FallbackAll}
	p, err := PlanRoute(ctx, o, ParentRoute{Fence: route.Fence{Open: true}})
	if err != nil {
		t.Fatal(err)
	}
	lead := Dispatch{Agent: &fakeRunner{name: o.Agent}, Model: o.Model, Effort: "high", Route: rowRoute(reg, o.Lead),
		Account: workspaceAccount(reg, o.Lead), Billing: "metered", otherRoute: true, swap: true, routerChose: true, rung: 3,
		keepCaller: true, servedRepick: func(string) string { return "repick" }}
	q := RouteReq{Agent: o.Agent, Model: o.Model, Fallback: o.Fallback}
	assembly := newPlannedChain(context.WithValue(withPlan(ctx, p), routeRequestKey{}, q), reg)
	c := assembly.append(Chain{Want: o.Agent}, lead)
	if len(c.Dispatches) != 3 || c.OwnHop() != 2 {
		t.Fatalf("lead admission = %+v, want Pi on both workspaces then own CLI", c)
	}
	head, own := c.Dispatches[0], c.Dispatches[2]
	if head.runFor != o.Agent || head.Model != "databricks-claude-sonnet-5-5" || head.Route != lead.Route ||
		head.Effort != lead.Effort || head.Billing != lead.Billing || !head.otherRoute || !head.swap || !head.routerChose || head.rung != 3 {
		t.Fatalf("runner metadata = %+v, lead %+v", head, lead)
	}
	if !own.keepCaller || own.servedRepick("") != "repick" || own.Agent != lead.Agent {
		t.Fatalf("own CLI metadata changed: %+v", own)
	}
	tail := Dispatch{Agent: &fakeRunner{name: "codex"}, Account: "openai·login", quotaOverflow: true}
	c = assembly.append(c, tail)
	if len(c.Dispatches) != 4 || !c.Dispatches[3].quotaOverflow || c.Dispatches[0].Agent != head.Agent {
		t.Fatalf("tail admission reordered the prefix: %+v", c)
	}
}
