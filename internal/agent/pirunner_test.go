package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A databricks:<p> confinement plans no pi hop on a sibling workspace and mints no token for one.
func TestPiRunnerConfinedStaysOnWorkspace(t *testing.T) {
	reg, _, _ := autoFixture(t)
	q := RouteReq{Agent: "claude", Model: "databricks-claude-sonnet-5-5", Fallback: router.FallbackAll}
	lead := Dispatch{Account: "databricks·dev", Model: q.Model}
	open := proc.WithValues(context.Background(), proc.Values{})
	sibs, skips, _ := piSiblings(open, reg, q, lead, q.Model)
	if len(sibs)+len(skips) == 0 {
		t.Fatal("fixture: an unconfined ask considers no sibling workspace")
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	if sibs, skips, why := piSiblings(armed, reg, q, lead, q.Model); len(sibs)+len(skips) != 0 || why != "" {
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

// ledBy with no hops leaves the chain as it was instead of indexing an empty list.
func TestLedByNoHops(t *testing.T) {
	c := Chain{Want: "claude", Dispatches: []Dispatch{{Model: "m"}}, Preflight: &Preflight{Chosen: "x"}}
	got := c.ledBy(nil)
	if len(got.Dispatches) != 1 || got.Preflight.Chosen != "x" {
		t.Fatalf("ledBy(nil) = %+v", got)
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
	if why := piRunnerBarred(ctx, RouteReq{Agent: "codex"}); !strings.Contains(why, "carried") {
		t.Fatalf("carried codex child: barred = %q, want the carried reason", why)
	}
	if why := piRunnerBarred(ctx, RouteReq{Agent: "gemini"}); why != "" {
		t.Fatalf("another agent under a codex carrier is barred: %q", why)
	}
	done, plain := record(1, true, "ok", os.Getppid()), record(2, false, "running", os.Getppid())
	noPID := record(3, true, "running", 0)
	for _, id := range []string{done.ID, plain.ID, noPID.ID, "../" + rec.ID} {
		ctx := proc.WithValues(context.Background(), proc.Values{proc.DispatchIDEnv: id})
		if why := piRunnerBarred(ctx, RouteReq{Agent: "codex"}); why != "" {
			t.Fatalf("parent %q is no live carrier, yet barred: %q", id, why)
		}
	}
}

// Every roster agent but pi takes the pi runner untyped; an unknown name never does.
func TestPiRunnerAskedForRosterAgents(t *testing.T) {
	setConfigDir(t)
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		if asked, typed := piRunnerAsked(context.Background(), RouteReq{Agent: name}); !asked || typed {
			t.Errorf("%s: asked %v typed %v, want asked untyped", name, asked, typed)
		}
	}
	for _, name := range []string{"pi", "persona"} {
		if asked, _ := piRunnerAsked(context.Background(), RouteReq{Agent: name}); asked {
			t.Errorf("%s took the pi runner", name)
		}
	}
}

// A rotation's kept-out rows get no sibling pi hop, hop or skip.
func TestPiSiblingsSkipRotationDrop(t *testing.T) {
	reg, _, _ := autoFixture(t)
	q := RouteReq{Agent: "claude", Model: "databricks-claude-sonnet-5-5", Fallback: router.FallbackAll}
	lead := Dispatch{Account: "databricks·dev", Model: q.Model}
	open := proc.WithValues(context.Background(), proc.Values{})
	sibs, skips, _ := piSiblings(open, reg, q, lead, q.Model)
	if len(sibs)+len(skips) == 0 {
		t.Fatal("fixture: an unrotated ask considers no sibling workspace")
	}
	var drop []string
	for name := range reg.Providers {
		if name != "dev" {
			drop = append(drop, name)
		}
	}
	ctx := router.WithRotatedLead(open, router.RotatedLead{Row: "dev", Drop: drop})
	if sibs, skips, why := piSiblings(ctx, reg, q, lead, q.Model); len(sibs)+len(skips) != 0 || why != "" {
		t.Fatalf("kept-out rows still planned pi hops: hops %v, skips %v, why %q", sibs, skips, why)
	}
}

// A plain pi ask follows the default only when no fallback mode is set anywhere; a typed none, pin or model is one hop.
func TestPiChainModeDefault(t *testing.T) {
	setConfigDir(t)
	ctx := context.Background()
	for _, c := range []struct {
		q    RouteReq
		want string
	}{
		{RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true}, router.FallbackAgents},
		{RouteReq{Agent: "pi", Fallback: router.FallbackNone}, ""},
		{RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true, Model: "m"}, ""},
		{RouteReq{Agent: "pi", Fallback: router.FallbackPin, FallbackDefaulted: true}, router.FallbackPin},
		{RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true, Resumed: true}, ""},
		{RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true, AutoModel: true}, ""},
		{RouteReq{Agent: "pi", Fallback: router.FallbackAgents, Native: true}, ""},
		{RouteReq{Agent: "pi", Fallback: router.FallbackAgents}, router.FallbackAgents},
	} {
		if got, _ := piChainMode(ctx, c.q); got != c.want {
			t.Errorf("piChainMode(%+v) = %q, want %q", c.q, got, c.want)
		}
	}
	t.Setenv(router.EnvFallback, router.FallbackNone)
	if got, _ := piChainMode(ctx, RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true}); got != "" {
		t.Errorf("an env none gave mode %q, want a one-hop chain", got)
	}
}

// The claude hop sits right after the pi hops, ahead of an off-arm claude hop; nothing follows it that was not there.
func TestAfterPiHopsOrder(t *testing.T) {
	pi, cl := &fakeRunner{name: "pi"}, &fakeRunner{name: "claude"}
	hops := []Dispatch{{Agent: pi, Account: "databricks·dev"}, {Agent: cl, Account: "anthropic·ultra"}}
	got := afterPiHops(hops, Dispatch{Agent: cl, Account: "databricks·dev"})
	if len(got) != 3 || got[1].Account != "databricks·dev" || got[2].Account != "anthropic·ultra" || len(hops) != 2 {
		t.Fatalf("hops = %+v", got)
	}
}

// A plain pi ask gets pi hops on the other workspaces after its lead; a confinement or no hop leaves it alone.
func TestWithPiSiblings(t *testing.T) {
	reg, _, _ := autoFixture(t) // the fixture roster has no pi driver, so each sibling is planned and then skipped
	q := RouteReq{Agent: "pi"}
	c := Chain{Dispatches: []Dispatch{{Agent: &fakeRunner{name: "pi"}, Account: "databricks·dev", Model: "databricks-gemini-3-8-flash"}}}
	open := proc.WithValues(context.Background(), proc.Values{})
	got := withPiSiblings(open, reg, q, c, router.FallbackAgents)
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
	if got := withPiSiblings(armed, reg, q, c, router.FallbackAgents); len(got.Dispatches) != 1 || got.Preflight != nil {
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
	q := RouteReq{Agent: "pi", Fallback: router.FallbackAgents}
	pi := &fakeRunner{name: "pi"}
	c := Chain{Dispatches: []Dispatch{{Agent: pi, Account: "databricks·dev", Model: "databricks-gemini-3-8-flash"}}}
	open := proc.WithValues(context.Background(), proc.Values{})
	got := withPiClaudeHop(open, q, c)
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
	pinned, err := piChainMode(open, RouteReq{Agent: "pi", Fallback: router.FallbackPin})
	if pinned != router.FallbackPin || err != nil {
		t.Fatalf("plain pin ask mode = %q, %v", pinned, err)
	}
	var pseq []string
	for _, d := range withPiClaudeHop(open, RouteReq{Agent: "pi", Fallback: router.FallbackPin}, c).Dispatches {
		pseq = append(pseq, d.Agent.Name()+" "+d.Account)
	}
	if pwant := []string{"pi databricks·dev", "claude databricks·dev"}; !slices.Equal(pseq, pwant) {
		t.Fatalf("pin hops = %v, want %v", pseq, pwant)
	}
	stray := Chain{Dispatches: []Dispatch{c.Dispatches[0], {Agent: pi, Account: "databricks·qa"}}}
	if got := withPiClaudeHop(open, RouteReq{Agent: "pi", Fallback: router.FallbackPin}, stray); len(got.Dispatches) != 2 {
		t.Fatalf("pin kept a stray workspace hop: %+v", got.Dispatches)
	}
	bad := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{bad`})
	one := withPiClaudeHop(bad, RouteReq{Agent: "pi", Fallback: router.FallbackNone, FallbackDefaulted: true}, c)
	if len(one.Dispatches) != 1 || one.Preflight == nil || !strings.Contains(strings.Join(one.Preflight.Notes, "\n"), "overlay unreadable") {
		t.Fatalf("a malformed overlay widened or went unnoted: %+v", one)
	}
	armed := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev"}`})
	if got := withPiClaudeHop(armed, q, c); len(got.Dispatches) != 2 || got.Dispatches[1].Agent.Name() != "claude" {
		t.Fatalf("a confined ask's hops = %+v, want pi then claude", got.Dispatches)
	}
}

// Any one of the overlay, the process env and the vault setting a fallback mode ends the default.
func TestFallbackUnsetSources(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	ctx := context.Background()
	if u, err := fallbackUnset(ctx); !u || err != nil {
		t.Fatalf("nothing set anywhere = %v, %v", u, err)
	}
	if u, err := fallbackUnset(proc.WithValues(ctx, proc.Values{route.OverlayVar: `{bad`})); u || err == nil {
		t.Errorf("a malformed overlay = %v, %v, want false with an error", u, err)
	}
	if u, _ := fallbackUnset(proc.WithValues(ctx, proc.Values{route.OverlayVar: `{"fallback":"agents"}`})); u {
		t.Error("an overlay fallback reads as unset")
	}
	if u, _ := fallbackUnset(proc.WithValues(ctx, proc.Values{router.EnvFallback: "agents"})); u {
		t.Error("a process env fallback reads as unset")
	}
	vaulttest.Put(t, vault.Env, router.EnvFallback, "pin")
	if u, _ := fallbackUnset(ctx); u {
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
		if why := piRunnerBarred(ctx, RouteReq{Agent: "codex", Opts: o}); why == "" {
			t.Errorf("%s: want a reason", name)
		}
	}
	if why := piRunnerBarred(ctx, RouteReq{Agent: "codex"}); why != "" {
		t.Errorf("plain ask barred: %q", why)
	}
}
