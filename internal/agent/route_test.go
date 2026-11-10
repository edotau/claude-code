package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An explicit backend stays one dispatch, records its own quota failure, and is skipped without an attempt after.
func TestExplicitChainRecordsAndGates(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	codex := &fakeRunner{name: "codex", errs: []error{errors.New("insufficient_quota: you exceeded your current quota")}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "openai"}
	c := explicitChain(codex, req)
	if len(c.Dispatches) != 1 || c.Dispatches[0].Account != "openai·openai" {
		t.Fatalf("chain = %+v, want one dispatch on openai·openai", c)
	}
	if _, err := RunWithFallback(context.Background(), c, req, wire.Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["openai·openai"]; e.Status(time.Now()) != router.StateExhausted {
		t.Fatalf("openai·openai not marked: %+v", e)
	}
	again := explicitChain(codex, req)
	if len(again.Dispatches) != 0 || len(again.Skipped) != 1 || !strings.Contains(again.Skipped[0], "openai·openai exhausted") {
		t.Fatalf("second explicit chain = %+v, want a gate skip", again)
	}
	if len(codex.seen) != 1 {
		t.Errorf("attempts = %d, want 1 (the gate stops the second)", len(codex.seen))
	}
}

// --native opencode leads on opencode's own config, ahead of (and instead of) the hub cell.
func TestRouteChainNativeOpencodeLeads(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	activateAgents(t, "codex")
	if got := codexEffortAt(t, "opus"); got != "high" {
		t.Fatalf("opus control effort = %q, want high", got)
	}
	if got := codexEffortAt(t, "sonnet"); got != "" {
		t.Errorf("sonnet effort = %q, must not be upgraded to opus's high", got)
	}
}

// codexEffortAt routes a codex request at alias and returns the first dispatch's effort, which must run the shared model.
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
	if _, err := RunWithFallback(ctx, c, RunOptions{Prompt: "x", Model: astra}, wire.Discard, nil); err != nil {
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

func astraRoute(t *testing.T) Chain {
	t.Helper()
	c, _, err := RouteChainFor(context.Background(), "codex", astra, "sonnet", router.FallbackAgents, false, router.PickOpts{Configured: allConfigured})
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
	c := astraRoute(t)
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
	c := astraRoute(t)
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

func TestModelRow(t *testing.T) {
	cases := []struct{ in, alias, row, exact string }{
		{"opus", "opus", "claude-opus-5-5", ""},
		{"claude-sonnet-5.5", "sonnet", "claude-sonnet-5-5", ""},
		{"databricks-claude-haiku-5-5", "haiku", "claude-haiku-5-5", ""},
		{"gpt-6.1-sol", "", "", "gpt-6-1-sol"},
		{"glm-5.3", "", "", "glm-5-3"},
		{"no-such-model", "", "", ""},
	}
	for _, c := range cases {
		alias, row, exact := modelRow(c.in)
		if alias != c.alias || row != c.row || exact != c.exact {
			t.Errorf("modelRow(%q) = (%q, %q, %q), want (%q, %q, %q)", c.in, alias, row, exact, c.alias, c.row, c.exact)
		}
	}
}

// A non-Claude id keeps matching agents, with Claude last at the lowest alias serving the id.
func TestRouteChainForNonClaudeIDIsExact(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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

// servedEnv seeds a hermetic vault with a signed-in Databricks row "dev", the switch on or off, a two-model Responses
// menu and (when cache is non-nil) a fresh listing of it; it returns the loaded registry.
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	b, m := retry.DefaultBaseDelay, retry.DefaultMaxDelay
	retry.DefaultBaseDelay, retry.DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { retry.DefaultBaseDelay, retry.DefaultMaxDelay = b, m })
	e429 := errors.New("rate limited: codex: exit status 1 exceeded retry limit, last status: 429 Too Many Requests")
	codex := &fakeRunner{name: "codex", errs: []error{e429, e429, e429}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "chatgpt"}
	if _, err := RunWithFallback(context.Background(), explicitChain(codex, req), req, wire.Discard, nil); err == nil {
		t.Fatal("the 429 must surface")
	}
	e := loadState()["openai·chatgpt"]
	if now := time.Now(); e.Status(now) != router.StateExhausted || e.ExhaustedUntil.After(now.Add(11*time.Minute)) {
		t.Fatalf("openai·chatgpt = %+v, want exhausted for about 10m", e)
	}
	api := &fakeRunner{name: "codex", errs: []error{e429, e429, e429}, emptyAnswer: true}
	paid := RunOptions{Prompt: "p", Provider: "openai"} // credits billing: one endpoint's 429 must not bench the account
	_, _ = RunWithFallback(context.Background(), explicitChain(api, paid), paid, wire.Discard, nil)
	if e := loadState()["openai·openai"]; !e.ExhaustedUntil.IsZero() {
		t.Fatalf("openai·openai = %+v, want no mark on a credits 429", e)
	}
}

// KeepCaller lets the agent_test batch tests build a dispatch that keeps the caller's own --model.
func KeepCaller(d Dispatch) Dispatch { d.keepCaller = true; return d }

// SwapLoadState replaces the router state loader with wrap(prev) and returns the restore func.
func SwapLoadState(wrap func(prev func() router.State) func() router.State) func() {
	prev := loadState
	loadState = wrap(prev)
	return func() { loadState = prev }
}

// CountAccountMarkers counts live marker files in dir whose content is account.
func CountAccountMarkers(dir, account string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.TrimSpace(string(b)) == account {
			n++
		}
	}
	return n
}

// SamplePeak counts account markers every interval until stop closes and returns the peak.
func SamplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(CountAccountMarkers(dir, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// leadRegistry has Databricks row dev (signed in) and Anthropic max; claude's record pins max.
func leadRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.SetRecord(t, "claude", "profile", "max")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// With a lead, the wanted agent's lead row is its own route and its pin is the other route.
func TestOtherRouteFollowsLead(t *testing.T) {
	rc := routeCtx{want: "codex", lead: "dev", pins: map[string]router.PinSpec{"codex": {Profile: "pin"}}}
	if rc.otherRoute(router.Candidate{Agent: "codex", Profile: "dev"}) {
		t.Error("lead row marked as another route")
	}
	if !rc.otherRoute(router.Candidate{Agent: "codex", Profile: "pin"}) {
		t.Error("pin row under a lead not marked as another route")
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
	}{
		{RouteReq{Agent: "codex", Fallback: all}, "", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all}, "dev", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: none}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all, Pinned: true}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: LeadSession, Fallback: none}, "dev", false, ""},
		{RouteReq{Agent: "codex", Provider: "databricks", Fallback: none}, "dev", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Lead: "max", Pinned: true}, "max", false, ""},
		{RouteReq{Agent: "codex", Model: "dev:opus", Fallback: all}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: none}, "", true, "nosuch"},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: all}, "", false, "nosuch"},
	}
	for _, c := range cases {
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
	t.Setenv(providers.EnvSessionProfile, "max")
	cases := []struct {
		q    RouteReq
		lead string
	}{
		{RouteReq{Agent: "codex", Fallback: router.FallbackAll, Preview: true}, ""},
		{RouteReq{Agent: "codex", Model: "max:opus", Fallback: router.FallbackAll, Preview: true}, "max"},
		{RouteReq{Agent: "codex", Model: "dev:opus", Profile: "max", Fallback: router.FallbackAll, Preview: true}, "max"},
	}
	for _, c := range cases {
		lead, explicit, err := routeLead(c.q)
		if lead != c.lead || explicit || err != nil {
			t.Errorf("%+v: got %q, %v, %v; want lead %q", c.q, lead, explicit, err, c.lead)
		}
	}
}

// The retired HARNESS_DATABRICKS_ARM env no longer moves an unflagged dispatch off its pin.
func TestRouteLeadIgnoresRetiredArmEnv(t *testing.T) {
	leadRegistry(t)
	t.Setenv("HARNESS_DATABRICKS_ARM", "dev")
	lead, explicit, err := routeLead(RouteReq{Agent: "codex", Fallback: router.FallbackAll})
	if lead != "" || explicit || err != nil {
		t.Errorf("got %q, %v, %v; want no lead", lead, explicit, err)
	}
}

// pinFirstSetup configures codex pinned to openai with opencode and claude behind it, each swapped for a stub.
func pinFirstSetup(t *testing.T, exhausted bool) (Chain, router.PickResult, map[string]*pinFallbackRunner) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key") // the non-own-login openai profile needs a credential
	vaulttest.SetRecord(t, "claude", "profile", "max")
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	vaulttest.SetRecord(t, "copilot", "profile", "")
	vaulttest.SetRecord(t, "gemini", "profile", "")
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	if exhausted {
		now := time.Now()
		if err := router.MarkExhausted("anthropic·max", now.Add(time.Hour), now, "test quota"); err != nil {
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
	for _, name := range []string{"codex", "opencode"} {
		if fakes[name] == nil {
			t.Fatalf("missing %s dispatch: %+v; skipped %v", name, c.Dispatches, c.Skipped)
		}
	}
	return c, pick, fakes
}

func TestPinFirstFallbackDispatches(t *testing.T) {
	c, pick, _ := pinFirstSetup(t, false)
	var got []string
	for _, disp := range c.Dispatches {
		got = append(got, disp.Agent.Name()+" ("+disp.Account+")")
	}
	want := []string{"codex (openai·openai)", "opencode (opencode·go)", "claude (anthropic·max)"}
	if !slices.Equal(got, want) {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	if c.Dispatches[0].Model != "openai:gpt-6.1-sol" || c.Dispatches[2].Model != "sonnet" {
		t.Errorf("models = %q, %q; want pinned codex and claude at sonnet", c.Dispatches[0].Model, c.Dispatches[2].Model)
	}
	for _, name := range []string{"copilot", "gemini"} {
		if !slices.ContainsFunc(pick.Skipped, func(s router.Skip) bool { return s.Agent == name && s.Gate == router.GateOff }) {
			t.Errorf("%s missing off gate: %+v", name, pick.Skipped)
		}
		containsAll(t, strings.Join(c.Skipped, "; "), name+": deactivated ("+strings.ToUpper(name)+"_ACTIVE=off)")
	}
}
