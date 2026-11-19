package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// --native opencode leads on opencode's own config, ahead of (and instead of) the hub cell.
func TestRouteChainNativeOpencodeLeads(t *testing.T) {
	setConfigDir(t)
	all := router.PickOpts{Configured: allConfigured}
	c, _, err := RouteChainFor(context.Background(), "opencode", "sonnet", "", router.FallbackAll, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Dispatches[0].Agent.(cliEngine); !ok || !c.Dispatches[0].unroutedDispatch {
		t.Fatalf("first dispatch = %+v, want the native opencode agent, unrouted", c.Dispatches[0])
	}
	for _, l := range c.Dispatches[1:] {
		if l.Agent.Name() == "opencode" {
			t.Errorf("the hub opencode cell must not also run: %+v", c.Dispatches)
		}
	}
	none, _, _ := RouteChainFor(context.Background(), "opencode", "sonnet", "", router.FallbackNone, true, all)
	if len(none.Dispatches) != 1 {
		t.Errorf("fallback none = %d dispatches, want the native dispatch only", len(none.Dispatches))
	}
}

// A codex dispatch asked at sonnet never inherits opus's effort.
func TestRequestedAliasSurvivesSharedModel(t *testing.T) {
	vaulttest.UseKeyring(t)
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	activateAgents(t, "codex")
	if got := codexEffortAt(t, "opus"); got != "high" {
		t.Fatalf("opus control effort = %q, want high", got)
	}
	if got := codexEffortAt(t, "sonnet"); got != "" {
		t.Errorf("sonnet effort = %q, must not be upgraded to opus's high", got)
	}
}

// codexEffortAt routes a codex request at alias and returns the first dispatch's effort, which runs the shared model.
func codexEffortAt(t *testing.T, alias string) string {
	t.Helper()
	c, _, err := RouteChainFor(context.Background(), "codex", alias, "sonnet", router.FallbackNone, false, router.PickOpts{Configured: allConfigured})
	if err != nil || len(c.Dispatches) == 0 || c.Dispatches[0].Agent.Name() != "codex" {
		t.Fatalf("%s chain = %+v, err %v", alias, c, err)
	}
	if c.Dispatches[0].Model != "gpt-6.1-sol" {
		t.Fatalf("%s dispatch model = %q, want the shared gpt-6.1-sol", alias, c.Dispatches[0].Model)
	}
	return c.Dispatches[0].Effort
}

const astra = "gpt-6-astra"

// stubDispatches swaps every dispatch's engine for a capturing stub; stubs[i] backs c.Dispatches[i].
func stubDispatches(c *Chain) []*pinFallbackRunner {
	stubs := make([]*pinFallbackRunner, len(c.Dispatches))
	for i := range c.Dispatches {
		f := &pinFallbackRunner{name: c.Dispatches[i].Agent.Name(), res: wire.Result{Answer: "answer from " + c.Dispatches[i].Agent.Name()}}
		stubs[i], c.Dispatches[i].Agent = f, f
	}
	return stubs
}

func TestAstraModelIdentityOwnLogin(t *testing.T) {
	ctx := context.Background()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "")
	activateAgents(t, "codex")
	c, _, err := RouteChainFor(ctx, "codex", astra, "sonnet", router.FallbackAgents, false, router.PickOpts{Configured: allConfigured})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Dispatches) == 0 || c.Dispatches[0].Agent.Name() != "codex" {
		t.Fatalf("first dispatch is not codex: %+v; skipped %v", c.Dispatches, c.Skipped)
	}
	stubs := stubDispatches(&c)
	if _, err := runWithFallback(ctx, c, RunOptions{Prompt: "x", Model: astra}, wire.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if len(stubs[0].seen) != 1 {
		t.Fatalf("codex requests = %d, want 1", len(stubs[0].seen))
	}
	got := stubs[0].seen[0].Model
	if got != astra {
		t.Errorf("codex agent model = %q, want %q", got, astra)
	}
	if c.Dispatches[0].Model != got {
		t.Errorf("codex Dispatch.Model = %q but agent got %q: planned dispatch records the stale tier default, not the model run", c.Dispatches[0].Model, got)
	}
}

// astraProfile installs a signed-in Databricks session profile (sonnet -> gpt-6-1-sol) with menu as its opencode menu.
func astraProfile(t *testing.T, profile string, menu []providers.OpencodeModel) *providers.Provider {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", profile)
	activateAgents(t, "codex")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+profile+`":{"preset":"databricks",`+
		`"host":"https://profile.example.com","auth":{"type":"pat"},"models":{"sonnet":"gpt-6-1-sol"}}}}}`)
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers[profile]
	if p == nil {
		t.Fatal("profile absent")
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), "profile-auth")
	return p
}

// profileCodexDispatches are the codex dispatches pinned to profile.
func profileCodexDispatches(c Chain, profile string) []Dispatch {
	var out []Dispatch
	for _, disp := range c.Dispatches {
		if disp.Agent.Name() == "codex" && strings.HasPrefix(disp.Model, profile+":") {
			out = append(out, disp)
		}
	}
	return out
}

// astraRoute asks codex for astra on profile: only a requested row is a hop, the session profile is not one by itself.
func astraRoute(t *testing.T, profile string) Chain {
	t.Helper()
	c, _, err := RouteChainFor(context.Background(), "codex", astra, "sonnet", router.FallbackAgents, false,
		router.PickOpts{Configured: allConfigured, Lead: profile})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAstraProfileEligibilitySupportedRetained(t *testing.T) {
	const profile = "astra-ok-q4"
	p := astraProfile(t, profile, []providers.OpencodeModel{
		{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
		{ID: astra, Label: "GPT-6 Astra", Responses: true},
	})
	c := astraRoute(t, profile)
	want := profile + ":" + p.WireModel(astra)
	dispatches := profileCodexDispatches(c, profile)
	if len(dispatches) != 1 || dispatches[0].Model != want {
		t.Errorf("profile codex dispatches = %+v, want one at %q; skipped %v", dispatches, want, c.Skipped)
	}
	for _, s := range c.Skipped {
		if strings.HasPrefix(s, "codex: serves no "+astra) {
			t.Errorf("Responses-capable profile dropped by the exact-model cell check: %q", s)
		}
	}
}

func TestAstraProfileEligibilityUnsupportedSkipped(t *testing.T) {
	const profile = "astra-no-z8"
	astraProfile(t, profile, []providers.OpencodeModel{
		{ID: "gpt-6-1-sol", Label: "GPT-6.1 Sol", Responses: true},
		{ID: astra, Label: "GPT-6 Astra", Responses: false},
	})
	c := astraRoute(t, profile)
	if dispatches := profileCodexDispatches(c, profile); len(dispatches) != 0 {
		t.Errorf("profile without an astra Responses route kept codex dispatches: %+v", dispatches)
	}
	// RED if the skip text omits the profile: "codex: serves no gpt-6-astra" does not name the account.
	named := false
	for _, s := range c.Skipped {
		if strings.Contains(s, "codex") && strings.Contains(s, profile) {
			named = true
		}
	}
	if !named {
		t.Errorf("no skip names codex and profile %q: %v", profile, c.Skipped)
	}
}

// pinFallbackRunner is a stub engine: it records each request and returns res and err.
type pinFallbackRunner struct {
	name string
	res  wire.Result
	err  error
	seen []RunOptions
}

func (f *pinFallbackRunner) Name() string     { return f.name }
func (f *pinFallbackRunner) Caps() Caps       { return Caps{Model: Native, Effort: Native} }
func (f *pinFallbackRunner) Available() error { return nil }
func (f *pinFallbackRunner) Run(_ context.Context, req RunOptions, _ wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	return f.res, f.err
}

// A non-Claude id keeps matching agents, with Claude last at the lowest alias serving the id.
func TestRouteChainForNonClaudeIDIsExact(t *testing.T) {
	setConfigDir(t)
	all := router.PickOpts{Configured: allConfigured}
	vaulttest.Use(t)
	activateAgents(t, "codex")
	c, _, err := RouteChainFor(context.Background(), "codex", "gpt-6.1-sol", "sonnet", router.FallbackAll, false, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Dispatches) == 0 || c.Dispatches[0].Agent.Name() != "codex" || !c.Dispatches[0].keepCaller {
		t.Fatalf("dispatches = %+v, want codex first and keeping the caller's id", c.Dispatches)
	}
	for _, l := range c.Dispatches[1:] {
		if n := l.Agent.Name(); n == "copilot" {
			t.Errorf("%s got a dispatch for a model its cell does not serve", n)
		}
	}
	if last := c.Dispatches[len(c.Dispatches)-1]; last.Agent.Name() != "claude" || last.Model != "sonnet" {
		t.Errorf("last dispatch = %+v, want claude at sonnet (the lowest codex alias serving gpt-6.1-sol)", last)
	}
}

// An alias-less older row runs its own cell on every agent, not the current alias's.
func TestRouteChainForCanonicalRowUsesItsCell(t *testing.T) {
	setConfigDir(t)
	all := router.PickOpts{Configured: allConfigured}
	c, _, err := RouteChainFor(context.Background(), "copilot", "claude-sonnet-5.5", "", router.FallbackAll, false, all)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range c.Dispatches {
		if l.Agent.Name() == "claude" && l.Model != "sonnet" {
			t.Errorf("claude dispatch model = %q, want the sonnet alias", l.Model)
		}
	}
}

const servedRow = `{"routing":{"servedAware":%s},"providers":{"databricks":{"dev":{"preset":"databricks",` +
	`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// servedEnv seeds a hermetic vault with a signed-in Databricks row "dev", a two-model Responses menu and listing cache.
func servedEnv(t *testing.T, on string, cache map[string]string) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", strings.Replace(servedRow, "%s", on, 1))
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	menu := []providers.OpencodeModel{{ID: "m-a", Label: "A", Responses: true}, {ID: "m-b", Label: "B", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	if cache != nil {
		s := router.Served{Profile: "dev", FetchedAt: time.Now()}
		for _, id := range []string{"m-a", "m-b"} {
			if r, ok := cache[id]; ok {
				s.Models = append(s.Models, router.ServedModel{ID: id, Ready: r})
			}
		}
		if err := router.SaveServed(s); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

var servedCand = router.Candidate{Agent: "codex", Profile: "dev", Account: "databricks·dev"}

// A plain 429 (no quota wording) that exhausts the retries holds the account out of the picks for 10 minutes.
func TestRateLimitedHopMarksAccount(t *testing.T) {
	setConfigDir(t)
	fastDefaultBackoff(t)
	e429 := errors.New("rate limited: codex: exit status 1 exceeded retry limit, last status: 429 Too Many Requests")
	codex := &fakeRunner{name: "codex", errs: []error{e429, e429, e429}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "chatgpt"}
	if _, err := runWithFallback(context.Background(), explicitChain(codex, req), req, wire.Discard, nil); err == nil {
		t.Fatal("the 429 must surface")
	}
	e := loadState()["openai·chatgpt"]
	if now := time.Now(); e.Status(now) != router.StateExhausted || e.ExhaustedUntil.After(now.Add(11*time.Minute)) {
		t.Fatalf("openai·chatgpt = %+v, want exhausted for about 10m", e)
	}
	api := &fakeRunner{name: "codex", errs: []error{e429, e429, e429}, emptyAnswer: true}
	paid := RunOptions{Prompt: "p", Provider: "openai"} // credits billing: one endpoint's 429 must not bench the account
	_, _ = runWithFallback(context.Background(), explicitChain(api, paid), paid, wire.Discard, nil)
	if e := loadState()["openai·openai"]; !e.ExhaustedUntil.IsZero() {
		t.Fatalf("openai·openai = %+v, want no mark on a credits 429", e)
	}
}

// leadRegistry has Databricks row dev (signed in) and Anthropic ultra; claude's record pins ultra.
func leadRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.SetRecord(t, "claude", "profile", "ultra")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// With a lead, the wanted agent's lead row is its own route and its pin is the other route.
func TestOtherRouteFollowsLead(t *testing.T) {
	rc := routeCtx{ctx: context.Background(), want: "codex", lead: "dev", pins: map[string]router.PinSpec{"codex": {Profile: "pin"}}}
	if rc.otherRoute(router.Candidate{Agent: "codex", Profile: "dev"}) {
		t.Error("lead row marked as another route")
	}
	if !rc.otherRoute(router.Candidate{Agent: "codex", Profile: "pin"}) {
		t.Error("pin row under a lead not marked as another route")
	}
}

// Under overlay databricks:<p> the wanted agent on workspace <p> is the asked route, whatever its pin says.
func TestOtherRouteFollowsArm(t *testing.T) {
	t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"qa"}`)
	rc := routeCtx{ctx: context.Background(), want: "opencode", pins: map[string]router.PinSpec{"opencode": {Profile: "dev"}}}
	if rc.otherRoute(router.Candidate{Agent: "opencode", Profile: "qa"}) {
		t.Error("the coworker workspace marked as another route")
	}
	if !rc.otherRoute(router.Candidate{Agent: "codex", Profile: "qa"}) || !rc.otherRoute(router.Candidate{Agent: "opencode", Profile: "prod"}) {
		t.Error("another agent or workspace not marked as another route")
	}
}

// LeadFor: a named profile leads (D1) unless --fallback none or Pinned pins it; session and --provider always lead.
func TestLeadFor(t *testing.T) {
	leadRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "dev")
	all, none := router.FallbackAll, router.FallbackNone
	cases := []struct {
		q        RouteReq
		lead     string
		explicit bool
		err      string
		overlay  string // an overlay account names a row and beats the (here absent) session lead
	}{
		{RouteReq{Agent: "codex", Fallback: all}, "dev", false, "", ""}, // D3: the session leads an untyped ask
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all}, "dev", false, "", ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: none}, "", true, "", ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all, Pinned: true}, "", true, "", ""},
		{RouteReq{Agent: "codex", Profile: LeadSession, Fallback: none}, "dev", false, "", ""},
		{RouteReq{Agent: "codex", Provider: "databricks", Fallback: none}, "dev", false, "", ""},
		{RouteReq{Agent: "codex", Profile: "dev", Lead: "ultra", Pinned: true}, "ultra", false, "", ""},
		{RouteReq{Agent: "codex", Model: "dev:opus", Fallback: all}, "", true, "", ""},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: none}, "", true, "nosuch", ""},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: all}, "", false, "nosuch", ""},
		{RouteReq{Agent: "codex", Fallback: all}, "dev", false, "", `{"account":"dev"}`},
	}
	for _, c := range cases {
		if c.overlay != "" {
			t.Setenv(agentconfig.OverlayVar, c.overlay)
			t.Setenv(providers.EnvSessionProfile, "")
		}
		lead, explicit, err := LeadFor(c.q)
		errOK := (c.err == "") == (err == nil) && (err == nil || strings.Contains(err.Error(), c.err))
		if lead != c.lead || explicit != c.explicit || !errOK {
			t.Errorf("%+v: got %q, %v, %v", c.q, lead, explicit, err)
		}
	}
}

// A preview of a profile:model leads on that model's profile; an unflagged preview has no lead.
func TestRouteLeadPreviewKeepsModelProfile(t *testing.T) {
	leadRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "ultra")
	cases := []struct {
		q    RouteReq
		lead string
	}{
		{RouteReq{Agent: "codex", Fallback: router.FallbackAll, Preview: true}, ""},
		{RouteReq{Agent: "codex", Model: "ultra:opus", Fallback: router.FallbackAll, Preview: true}, "ultra"},
		{RouteReq{Agent: "codex", Model: "dev:opus", Profile: "ultra", Fallback: router.FallbackAll, Preview: true}, "ultra"},
	}
	for _, c := range cases {
		lead, explicit, err := routeLeadContext(context.Background(), c.q)
		if lead != c.lead || explicit || err != nil {
			t.Errorf("%+v: got %q, %v, %v; want lead %q", c.q, lead, explicit, err, c.lead)
		}
	}
}

// The retired HARNESS_DATABRICKS_ARM env no longer moves an unflagged dispatch off its pin.
func TestRouteLeadIgnoresRetiredArmEnv(t *testing.T) {
	leadRegistry(t)
	t.Setenv("HARNESS_DATABRICKS_ARM", "dev")
	lead, explicit, err := routeLeadContext(context.Background(), RouteReq{Agent: "codex", Fallback: router.FallbackAll})
	if lead != "" || explicit || err != nil {
		t.Errorf("got %q, %v, %v; want no lead", lead, explicit, err)
	}
}

// pinFirstSetup configures codex pinned to openai with claude behind it (opencode is no hop), each swapped for a stub.
func pinFirstSetup(t *testing.T, exhausted bool) (Chain, router.PickResult, map[string]*pinFallbackRunner) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key") // the non-own-login openai profile needs a credential
	vaulttest.SetRecord(t, "claude", "profile", "ultra")
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	vaulttest.SetRecord(t, "copilot", "profile", "")
	vaulttest.SetRecord(t, "gemini", "profile", "")
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	if exhausted {
		now := time.Now()
		if err := router.MarkExhausted("anthropic·ultra", now.Add(time.Hour), now, "test quota"); err != nil {
			t.Fatal(err)
		}
	}
	c, pick, err := RouteChainFor(context.Background(), "codex", "", "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured})
	if err != nil {
		t.Fatal(err)
	}
	fakes := make(map[string]*pinFallbackRunner)
	for i := range c.Dispatches {
		f := &pinFallbackRunner{name: c.Dispatches[i].Agent.Name(), res: wire.Result{Answer: "answer from " + c.Dispatches[i].Agent.Name()}}
		if fakes[f.name] != nil {
			t.Fatalf("duplicate dispatch %q: %+v", f.name, c.Dispatches)
		}
		fakes[f.name], c.Dispatches[i].Agent = f, f
	}
	if fakes["codex"] == nil {
		t.Fatalf("missing codex dispatch: %+v; skipped %v", c.Dispatches, c.Skipped)
	}
	return c, pick, fakes
}

func TestPinFirstFallbackDispatches(t *testing.T) {
	c, pick, _ := pinFirstSetup(t, false)
	var got []string
	for _, disp := range c.Dispatches {
		got = append(got, disp.Agent.Name()+" ("+disp.Account+")")
	}
	want := []string{"codex (openai·openai)", "claude (anthropic·ultra)"} // requested (= the pin), then claude
	if !slices.Equal(got, want) {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	if c.Dispatches[0].Model != "openai:gpt-6.1-sol" || c.Dispatches[1].Model != "sonnet" {
		t.Errorf("models = %q, %q; want pinned codex and claude at sonnet", c.Dispatches[0].Model, c.Dispatches[1].Model)
	}
	for _, name := range []string{"copilot", "gemini", "opencode"} { // never in the order, so never a skip reason
		if slices.ContainsFunc(pick.Skipped, func(s router.Skip) bool { return strings.HasPrefix(s.Agent, name) }) {
			t.Errorf("%s is off the order but listed as skipped: %+v", name, pick.Skipped)
		}
		if strings.Contains(strings.Join(c.Skipped, "; "), name) {
			t.Errorf("%s is off the order but in the chain's skips: %v", name, c.Skipped)
		}
	}
}

// A pinned caller's own profile:model runs the bare model in its cell; buildDispatch adds the profile once.
func TestPinnedCellStripsOwnProfile(t *testing.T) {
	rc := routeCtx{ctx: context.Background(), want: "codex", explicitModel: true, callerModel: "dev:databricks-gpt-6-1-sol",
		pins: map[string]router.PinSpec{"codex": {Profile: "dev"}}}
	cell, skip := rc.candidateCell(router.Candidate{Agent: "codex", Profile: "dev"})
	if skip != "" || cell.Model != "databricks-gpt-6-1-sol" {
		t.Errorf("cell model %q skip %q, want databricks-gpt-6-1-sol", cell.Model, skip)
	}
}

// profileCell refuses an exact model the fresh listing has NOT_READY, with the new message.
func TestProfileCellRejectsNotReadyExactModel(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "NOT_READY", "m-b": "READY"})
	_, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if !strings.Contains(why, "m-a is not a READY endpoint on dev") {
		t.Fatalf("skip reason = %q, want the not-READY message", why)
	}
	if cell, why := (routeCtx{reg: reg, exact: "m-b"}).profileCell(servedCand, router.Cell{}); why != "" || cell.Model != "m-b" {
		t.Errorf("READY m-b = %+v, %q, want accepted", cell, why)
	}
}

// a cold cache accepts the exact model on the static Responses check, even with the switch on.
func TestProfileCellAcceptsStaticallyOnColdCache(t *testing.T) {
	reg := servedEnv(t, "true", nil)
	cell, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if why != "" || cell.Model != "m-a" {
		t.Fatalf("cold cache = %+v, %q, want m-a accepted", cell, why)
	}
}

// copilotOnDev routes copilot (GitHub pin gpt-6.1-sol) on served-aware dev, glm-5-3 NOT_READY; returns both.
func copilotOnDev(t *testing.T, model string) (dev []string, skipped []string) {
	t.Helper()
	reg := servedEnv(t, "true", nil)
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Ready: "READY"}, {ID: "databricks-gpt-6-1-sol", Ready: "READY"},
		{ID: "databricks-kimi-k3", Ready: "READY"}, {ID: "databricks-glm-5-3", Ready: "NOT_READY"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	activateAgents(t, "copilot")
	vaulttest.SetRecord(t, "copilot", "model", "gpt-6.1-sol")
	c, _, err := RouteChainFor(context.Background(), "copilot", model, "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured, Lead: "dev"})
	if err != nil || reg.Providers["dev"] == nil {
		t.Fatal(err)
	}
	for _, d := range c.Dispatches {
		if d.Agent.Name() == "copilot" && strings.HasPrefix(d.Model, "dev:") {
			dev = append(dev, d.Model)
		}
	}
	return dev, c.Skipped
}

// An explicit Claude id runs on the lead profile as asked, not swapped for the agent's model pin.
func TestCallerClaudeIDBeatsModelPinOnLeadProfile(t *testing.T) {
	dev, skipped := copilotOnDev(t, "databricks-claude-opus-5-5")
	if len(dev) != 1 || dev[0] != "dev:databricks-claude-opus-5-5" {
		t.Fatalf("dev dispatches = %v (skipped %v), want dev:databricks-claude-opus-5-5", dev, skipped)
	}
}

// A served non-Claude id is a direct hit on the lead profile; an unserved one is skipped under its own spelling.
func TestCallerExactIDServedOnLeadProfile(t *testing.T) {
	dev, skipped := copilotOnDev(t, "databricks-kimi-k3")
	if len(dev) != 1 || dev[0] != "dev:databricks-kimi-k3" || slices.ContainsFunc(skipped, isCopilotDevSkip) {
		t.Fatalf("kimi: dev dispatches = %v, skipped %v; want one direct dispatch, no dev skip", dev, skipped)
	}
	dev, skipped = copilotOnDev(t, "databricks-glm-5-3")
	if len(dev) != 0 || !slices.ContainsFunc(skipped, func(s string) bool {
		return isCopilotDevSkip(s) && strings.Contains(s, "serves no databricks-glm-5-3")
	}) {
		t.Fatalf("glm: dev dispatches = %v, skipped %v; want a dev skip naming databricks-glm-5-3", dev, skipped)
	}
}

func isCopilotDevSkip(s string) bool { return strings.HasPrefix(s, "copilot (databricks·dev):") }

// ENDPOINT_NOT_FOUND marks the model gone and returns the next READY Responses model.
func TestServedRepickOnEndpointNotFound(t *testing.T) {
	quietStderr(t)
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	disp := Dispatch{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if disp.servedRepick == nil {
		t.Fatal("codex on a Databricks profile with the switch on must get a repick")
	}
	err := errors.New("RESOURCE_DOES_NOT_EXIST: ENDPOINT_NOT_FOUND: no endpoint m-a")
	if got := retryModel(disp, RunOptions{Model: "dev:m-a"}, "", err); got != "dev:m-b" {
		t.Fatalf("repick = %q, want dev:m-b", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; at.IsZero() {
		t.Error("m-a was not marked unserved")
	}
	if got := retryModel(disp, RunOptions{Model: "dev:m-b"}, "", err); got != "" {
		t.Errorf("second failure repicked %q, want none left", got)
	}
}

// an error that is not an endpoint-gone answer neither repicks nor marks.
func TestServedRepickIgnoresUnrelatedError(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	disp := Dispatch{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if got := retryModel(disp, RunOptions{Model: "dev:m-a"}, "", errors.New("connection reset")); got != "" {
		t.Errorf("unrelated error repicked %q", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; !at.IsZero() {
		t.Error("an unrelated error must not mark m-a")
	}
}

// an exact --model, a non-codex dispatch and a switch-off run get no served repick.
func TestServedRepickNotOfferedWhereItWouldMislead(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	if (routeCtx{reg: reg, exact: "m-a"}.servedRepickFor(servedCand)) != nil {
		t.Error("exact --model must not repick")
	}
	copilot := servedCand
	copilot.Agent = "copilot"
	if (routeCtx{reg: reg}.servedRepickFor(copilot)) != nil {
		t.Error("a non-codex dispatch must not get a served repick")
	}
	off := servedEnv(t, "false", nil)
	if (routeCtx{reg: off}.servedRepickFor(servedCand)) != nil {
		t.Error("switch off must not repick")
	}
}

// A Progressed failure never repicks: the next model would re-run the prompt and repeat its tools.
func TestRetryModelSkipsProgressedError(t *testing.T) {
	disp := Dispatch{servedRepick: func(string) string { return "dbx:m-b" }}
	capMsg := errors.New("ENDPOINT_NOT_FOUND: no endpoint m-a")
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, "", &retry.Error{Kind: retry.KindFatal, Agent: "x", Progressed: true, Err: capMsg}); got != "" {
		t.Errorf("progressed capability error repicked %q, want none", got)
	}
	if got := retryModel(disp, RunOptions{Model: "dbx:m-a"}, "", &retry.Error{Kind: retry.KindFatal, Agent: "x", Err: capMsg}); got != "dbx:m-b" {
		t.Errorf("unprogressed capability error = %q, want dbx:m-b", got)
	}
}

func TestRouteChainOwnConnector(t *testing.T) {
	setConfigDir(t)
	for _, model := range []string{"opencode-go/kimi-k3", "opencode/kimi-k3"} {
		c, _, err := RouteChainFor(context.Background(), "opencode", model, "opus", router.FallbackAll, false, router.PickOpts{})
		if err != nil || len(c.Dispatches) != 1 {
			t.Fatalf("%s: chain=%+v err=%v", model, c, err)
		}
		d := c.Dispatches[0]
		if got := d.Agent.(fixtureHubEngine).firstOutput; got <= 0 || got != firstOutputDeadline() {
			t.Fatalf("%s: stall deadline = %v, want %v", model, got, firstOutputDeadline())
		}
		if d.Model != "kimi-k3" || !d.keepCaller || c.Preflight != nil || d.Agent.(HubEngine).HubProvider().OpencodeID != strings.Split(model, "/")[0] {
			t.Fatalf("%s: dispatch=%+v preflight=%+v", model, d, c.Preflight)
		}
	}
}

// A typed alias on a pinned row runs where that agent's pin can serve it, else keeps the pin's own cell.
func TestPinnedCallerAlias(t *testing.T) {
	setConfigDir(t)
	reg := routeRegistry(t)
	pinCell := router.Cell{Model: "pin-cell"}
	dev := reg.Providers["dev"]
	cases := []struct{ agent, profile, alias, want string }{
		{"opencode", "dev", "opus", router.DatabricksModel("opencode", "opus", dev)},
		{"opencode", "dev", "sonnet", router.DatabricksModel("opencode", "sonnet", dev)},
		{"codex", "dev", "opus", router.DatabricksModel("codex", "opus", dev)},
		{"codex", "dev", "sonnet", "pin-cell"},           // dev serves sonnet only as a Claude id, not over Responses
		{"copilot", "aueric_boehcp", "opus", "pin-cell"}, // an own-login row's cell already carries the alias
		{"gemini", "antigravity", "opus", "pin-cell"},
		{"gemini", "antigravity", "haiku", "pin-cell"},
	}
	for _, c := range cases {
		_, row, _ := router.ModelRow(c.alias)
		pins := map[string]router.PinSpec{c.agent: {Profile: c.profile, Model: "pin-cell"}}
		rc := routeCtx{want: c.agent, reg: reg, pins: pins, explicitModel: true, callerModel: c.alias, row: row}
		cand := router.Candidate{Agent: c.agent, Profile: c.profile, Cell: pinCell}
		if !rc.pinnedCaller(cand) {
			t.Fatalf("%s %s: alias refused on pin", c.agent, c.alias)
		}
		cell := pinCell
		rc.pinnedCell(cand, &cell)
		if cell.Model != c.want {
			t.Errorf("%s %s: model=%q want=%q", c.agent, c.alias, cell.Model, c.want)
		}
	}
}

// The claude ladder ends with agy's own Claude on the Google login, only in the modes that allow another agent.
func TestRouteChainAgyOverflow(t *testing.T) {
	vaulttest.Use(t)
	cases := []struct {
		want, alias, mode string
		hop               bool
	}{
		{"claude", "sonnet", router.FallbackAll, true},
		{"claude", "opus", router.FallbackAgents, true},
		{"claude", "sonnet", router.FallbackNone, false},
		{"claude", "sonnet", router.FallbackPin, false},
		{"claude", "haiku", router.FallbackAll, false}, // agy serves no Claude Haiku
		{"codex", "sonnet", router.FallbackAll, false},
	}
	for _, c := range cases {
		chain, _, err := RouteChainFor(context.Background(), c.want, "", c.alias, c.mode, false, router.PickOpts{Configured: allConfigured})
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		last := chain.Dispatches[len(chain.Dispatches)-1]
		if last.quotaOverflow != c.hop {
			t.Fatalf("%s %s %s: quotaOverflow = %v, want %v (%d dispatches)", c.want, c.alias, c.mode, last.quotaOverflow, c.hop, len(chain.Dispatches))
		}
		if id, _ := models.Alias(c.alias); c.hop && (last.Agent.Name() != "gemini" || last.Model != id || last.Account != "google·antigravity") {
			t.Errorf("%s: hop = %s %s on %s, want gemini %s on google·antigravity", c.alias, last.Agent.Name(), last.Model, last.Account, id)
		}
	}
}

const familyRows = `{"routing":{"servedAware":true},"providers":{"databricks":{"dev":{"preset":"databricks",` +
	`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}},"github":{"octo":{"auth":{"login":"copilot",` +
	`"type":"cli"},"base_url":"https://api.githubcopilot.com","login":"octo-user","protocol":"openai"}}}}`

// familyEnv seeds dev listing 6.1 and 6.2 Sol and Astra plus Claude, and counts reads of the Copilot list.
func familyEnv(t *testing.T) (*providers.Registry, *int) {
	t.Helper()
	setConfigDir(t)
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", familyRows)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	activateAgents(t, "codex")
	vaulttest.SetRecord(t, "copilot", "profile", "octo")
	for _, a := range []string{"codex", "copilot"} {
		pathstest.FakeVendor(t, a, "exit 0")
	}
	s := router.Served{Profile: "dev", FetchedAt: time.Now()}
	for _, id := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "gpt-6-1-sol", "gpt-6-2-sol", "gpt-6-astra", "gpt-6-2-astra"} {
		s.Models = append(s.Models, router.ServedModel{ID: "databricks-" + id, Ready: "READY"})
	}
	if err := router.SaveServed(s); err != nil {
		t.Fatal(err)
	}
	reads := 0
	prev := router.GithubCatalogIDs
	router.GithubCatalogIDs = func(*providers.Provider) []string { reads++; return []string{"gpt-6.1-sol", "gpt-6.2-sol"} }
	t.Cleanup(func() { router.GithubCatalogIDs = prev })
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg, &reads
}

// chainModels is every dispatch model of a chain, or the error text.
func chainModels(c Chain, err error) []string {
	if err != nil {
		return []string{"error: " + err.Error()}
	}
	var out []string
	for _, d := range c.Dispatches {
		out = append(out, d.Model)
	}
	return out
}

type familyCase struct {
	name string
	got  func() []string
}

// rungDispatches builds a dispatch from each family rung candidate of a want-less opus pick.
func rungDispatches(ctx context.Context, reg *providers.Registry) []string {
	tb, err := router.Load()
	if err != nil {
		return chainModels(Chain{}, err)
	}
	pick := tb.Pick(ctx, "opus", "", loadState(), router.PickOpts{Fallback: router.FallbackAll, Configured: allConfigured})
	rc, out := routeCtx{ctx: ctx, tb: tb, reg: reg, want: "copilot"}, []string(nil)
	for _, c := range pick.Candidates {
		if providers.IsFamily(c.Model) {
			d, _, err := rc.buildDispatch(c, c.Cell)
			out = append(out, chainModels(Chain{Dispatches: []Dispatch{d}}, err)...)
		}
	}
	return out
}

// familyEntryCases is each entry point (typed, row:family, overlay, pin, rung, bucket) on the direct and routed path.
func familyEntryCases(t *testing.T, reg *providers.Registry) []familyCase {
	ctx, dev := context.Background(), reg.Providers["dev"]
	direct := func(q ModelQuery) []string {
		r, err := ResolveModel(reg, q, time.Now())
		return append(chainModels(Chain{}, err), r.Model)
	}
	target := func(req RunOptions) []string {
		_, tg, err := resolveIn(reg, req)
		return append(chainModels(Chain{}, err), tg.Model)
	}
	routedOn := func(lead, want, model, alias string) []string {
		c, _, err := RouteChainFor(ctx, want, model, alias, router.FallbackAll, false, router.PickOpts{Configured: allConfigured, Lead: lead})
		return chainModels(c, err)
	}
	routed := func(want, model, alias string) []string { return routedOn("dev", want, model, alias) }
	preview := func(q RouteReq) []string { c, _, err := Resolve(ctx, q); return chainModels(c, err) }
	pinned := func(agent string, run func() []string) []string {
		vaulttest.SetRecord(t, agent, "model", "sol")
		defer vaulttest.SetRecord(t, agent, "model", "")
		return run()
	}
	return []familyCase{
		{"direct typed", func() []string { return target(RunOptions{Agent: "codex", Provider: "dev", Model: "sol"}) }},
		{"direct row:family", func() []string { return target(RunOptions{Agent: "codex", Model: "dev:sol"}) }},
		{"direct overlay", func() []string { return direct(ModelQuery{Agent: "codex", Profile: "dev", Model: "sol"}) }},
		{"direct pin", func() []string {
			return pinned("codex", func() []string { return []string{PinnedTarget(ctx, "codex", providers.Target{Provider: dev}).Model} })
		}},
		{"direct bucket", func() []string { return direct(ModelQuery{Agent: "codex", Profile: "dev", Model: "opus"}) }},
		{"routed typed", func() []string { return routed("codex", "sol", "sonnet") }},
		{"routed typed own login", func() []string { return routedOn("", "copilot", "sol", "sonnet") }},
		{"routed row:family", func() []string { return preview(RouteReq{Agent: "codex", Model: "dev:sol", Preview: true}) }},
		{"routed overlay", func() []string { return preview(RouteReq{Agent: "codex", Profile: "dev", Model: "sol", Preview: true}) }},
		{"routed pin", func() []string { return pinned("copilot", func() []string { return routed("copilot", "", "sonnet") }) }},
		{"routed served pin", func() []string {
			vaulttest.SetRecord(t, "codex", "profile", "dev")
			defer vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
			return pinned("codex", func() []string { return routedOn("", "codex", "", "sonnet") })
		}},
		{"routed ladder rung", func() []string { return rungDispatches(ctx, reg) }},
		{"routed bucket", func() []string { return routed("codex", "", "opus") }},
	}
}

// Past the row pairing no family name survives: every entry point on both paths hands on a concrete id.
func TestFamilyIsConcreteAtEveryEntry(t *testing.T) {
	reg, _ := familyEnv(t)
	for _, c := range familyEntryCases(t, reg) {
		got := c.got()
		if len(got) == 0 {
			t.Fatalf("%s: no dispatch", c.name)
		}
		first := got[0][strings.Index(got[0], ":")+1:] // a row-qualified dispatch model loses its "<row>:"
		if _, _, member := providers.FamilyVersion(first); !member {
			t.Errorf("%s: first hop runs %q, want a family member (all %v)", c.name, got[0], got)
		}
		for _, m := range got {
			if strings.HasPrefix(m, "error: ") || providers.FamilyName(m) != "" {
				t.Errorf("%s: %q is no concrete id (all %v)", c.name, m, got)
			}
		}
	}
}

// A routed request naming no family reads no first-party list; the Copilot rungs stay unresolved until run.
func TestRoutedConcreteReadsNoFamilyList(t *testing.T) {
	_, reads := familyEnv(t)
	for _, c := range []struct{ want, model, alias string }{
		{"codex", "", "sonnet"}, {"codex", "sonnet", ""}, {"codex", "gpt-6.1-sol", "sonnet"},
		{"copilot", "gpt-6.1-sol", "sonnet"}, {"copilot", "claude-sonnet-5.5", ""},
	} {
		if _, _, err := RouteChainFor(context.Background(), c.want, c.model, c.alias, router.FallbackAll, false,
			router.PickOpts{Configured: allConfigured}); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if *reads != 0 {
			t.Fatalf("%+v read the Copilot list %d times, want 0", c, *reads)
		}
	}
}

// A profile-scoped auto (or no model) for gemini is not refused by the opus alias it never runs.
func TestExplicitRouteErrAutoOnProfile(t *testing.T) {
	autoFixture(t)
	for _, q := range []RouteReq{{Agent: "gemini", Profile: "dev"}, {Agent: "gemini", Profile: "dev", Model: "dev:auto"}} {
		if err := explicitRouteErr(t.Context(), q); err != nil {
			t.Errorf("explicitRouteErr(%+v) = %v, want nil", q, err)
		}
	}
}

func TestCanLeadClaudeOnDatabricks(t *testing.T) {
	reg, _, _ := autoFixture(t)
	if !canLead("claude", reg.Row("dev")) {
		t.Error("claude cannot lead on a Databricks row; gateway --provider databricks says none serves it")
	}
}
