package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex served by an OpenAI API key bills openai·openai (credits), not the ChatGPT plan's key.
func TestResolveAccountNamesActualBackend(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "profile", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	acct, billing := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "codex:chatgpt"})
	if acct != "openai·openai" || billing != router.BillCredits {
		t.Fatalf("resolveAccount = (%q, %q), want openai·openai credits", acct, billing)
	}
	if a, _ := resolveAccount("opencode", router.Cell{Model: "opencode-go/kimi-k3.1-pro"}); a != "" {
		t.Errorf("opencode keeps its table account, got %q", a)
	}
}

// The session's profile (env) beats the vault pin, and a codex login cell keeps its own account.
func TestResolveAccountFollowsSessionProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	t.Setenv(providers.EnvSessionProfile, "openai")
	if acct, _ := resolveAccount("claude", router.Cell{Model: "claude-sonnet-5-5"}); acct != "openai·openai" {
		t.Errorf("claude account = %q, want the session profile openai·openai", acct)
	}
	if acct, _ := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "openai·chatgpt"}); acct != "" {
		t.Errorf("codex login cell account = %q, want it kept (empty override)", acct)
	}
}

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

// Skip notices name only the wanted agent's gates; other agents' reasons are not printed.
func TestSkipNoticeOnlyForWantedAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c := Single(&fakeRunner{name: "copilot", errs: []error{nil}})
	c.Skipped = []string{"opencode·openai: not configured", "copilot: exhausted"}
	var got []string
	if _, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, wire.Discard, func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Contains(got[0], "opencode") || !strings.Contains(got[0], "copilot: exhausted") {
		t.Errorf("notices = %q, want only the copilot skip", got)
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

func TestAstraModelIdentityZeroLegs(t *testing.T) {
	stub := &pinFallbackRunner{name: "codex"}
	_, err := RunWithFallback(context.Background(), Chain{Want: "codex"}, RunOptions{Prompt: "x", Model: astra}, wire.Discard, nil)
	var e *retry.Error
	if !errors.As(err, &e) || e.Kind != retry.KindFatal {
		t.Errorf("err = %v, want *Error KindFatal", err)
	}
	if len(stub.seen) != 0 {
		t.Errorf("agent calls = %d, want 0", len(stub.seen))
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

func TestClaudeCellModelIsTheAlias(t *testing.T) {
	if got := claudeCellModel("claude-opus-5-5"); got != "opus" {
		t.Errorf("claudeCellModel(claude-opus-5-5) = %q, want opus (so pins resolve it)", got)
	}
	if got := claudeCellModel("claude-opus-4-1"); got != "claude-opus-4-1" {
		t.Errorf("an alias-less row stays canonical, got %q", got)
	}
}

// A canonical Claude id takes the session's alias selection, so a CLAUDE_<TIER>_MODEL pin wins over the typed id.
func TestResolveTargetCanonicalIDTakesTheTierPin(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5-5"}}}}}`)
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "claude-sonnet-5")
	_, got, err := resolveTarget(RunOptions{Provider: "dev", Model: "claude-sonnet-5-5"})
	if err != nil || got.Model != "claude-sonnet-5" {
		t.Errorf("claude-sonnet-5-5 = %+v (%v), want the pinned claude-sonnet-5", got, err)
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

// A run on an agent's own login reports the registry profile naming that login, never the retired "own-login".
func TestOwnProfileNamesTheLoginRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := SchemaFor("codex").OwnProfile(); got == "own-login" || got == "" {
		t.Errorf("OwnProfile(codex) = %q", got)
	}
	if got := SchemaFor("opencode").OwnProfile(); got != "opencode" {
		t.Errorf("OwnProfile(opencode) = %q, want the agent name when no row names its login", got)
	}
}

type markCall struct{ account, reason string }

// authEnv seeds a default provider row and records route.MarkAuth calls instead of touching quota.json.
func authEnv(t *testing.T, row string) *[]markCall {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"agauth","providers":{"anthropic":{"agauth":`+row+`}}}`)
	var calls []markCall
	prev := markAuth
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth = prev })
	return &calls
}

const u2mNoToken = `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"oauth"},"models":{"opus":"m"}}`

func twoDispatches(first, second *fakeRunner) Chain {
	return Chain{Want: first.Name(), Dispatches: []Dispatch{{Agent: first, Account: first.Name() + ":agauth"}, {Agent: second}}}
}

// A4+B2: a sealed-auth provider gets a Recover; its login-required failure marks the dispatch's account and moves on.
func TestSealedAuthDispatchRecoversOnceThenMovesOn(t *testing.T) {
	calls := authEnv(t, u2mNoToken)
	first := &fakeRunner{name: "codex", errs: []error{errors.New("HTTP 401 unauthorized"), errors.New("HTTP 401 unauthorized")}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}

	res, err := RunWithFallback(context.Background(), twoDispatches(first, second), RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Answer != "a" {
		t.Fatalf("res=%+v err=%v, want the second dispatch's answer", res, err)
	}
	if len(first.seen) != 1 {
		t.Fatalf("first dispatch attempts = %d, want 1: a failed Recover must not retry", len(first.seen))
	}
	if len(second.seen) != 1 {
		t.Fatalf("second dispatch attempts = %d, want it to run in the same dispatch", len(second.seen))
	}
	if len(*calls) != 1 || (*calls)[0].account != "codex:agauth" || (*calls)[0].reason == "" {
		t.Fatalf("markAuth calls = %+v, want one mark on codex:agauth", *calls)
	}
	if res.Fallback != "codex" || !strings.Contains(res.FallbackReason, "codex") {
		t.Errorf("fallback not reported: %+v", res)
	}
}

// A transient Recover failure (the auth.command itself failing) never marks the account.
func TestTransientRecoverFailureDoesNotMark(t *testing.T) {
	calls := authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","command":"exit 1"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "codex", errs: []error{errors.New("HTTP 401 unauthorized")}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	_, _ = RunWithFallback(context.Background(), twoDispatches(first, second), RunOptions{Prompt: "p"}, wire.Discard, nil)
	if len(*calls) != 0 {
		t.Fatalf("a transient recover failure condemned an account: %+v", *calls)
	}
}

// A first-attempt error that itself carries a login-required verdict (no Recover ran) is condemned too.
func TestWrappedLoginRequiredOnFirstAttemptMarksAndMovesOn(t *testing.T) {
	calls := authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "claude", errs: []error{fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), twoDispatches(first, second), RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Answer != "a" || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v: expected a move-on to the second dispatch", res, err)
	}
	if len(*calls) != 1 || (*calls)[0].account != "claude:agauth" {
		t.Fatalf("markAuth calls = %+v", *calls)
	}
}

func TestCondemnAuthRules(t *testing.T) {
	needs := fmt.Errorf("x: %w", providers.ErrNeedsLogin)
	for name, c := range map[string]struct {
		e       *retry.Error
		recover error
		want    bool
	}{
		"auth + condemning recover":      {&retry.Error{Kind: retry.KindAuth, Err: errors.New("401")}, needs, true},
		"auth + transient recover":       {&retry.Error{Kind: retry.KindAuth, Err: errors.New("401")}, errors.New("exit status 1"), false},
		"auth + no recover run":          {&retry.Error{Kind: retry.KindAuth, Err: errors.New("401")}, nil, false},
		"fatal error carrying the login": {&retry.Error{Kind: retry.KindFatal, Err: needs}, nil, true},
		"fatal ordinary":                 {&retry.Error{Kind: retry.KindFatal, Err: errors.New("boom")}, needs, false},
		"transient ordinary":             {&retry.Error{Kind: retry.KindTransient, Err: errors.New("503")}, nil, false},
	} {
		condemnAuth(c.e, c.recover)
		if got := c.e.AuthReason != ""; got != c.want {
			t.Errorf("%s: condemned = %v, want %v", name, got, c.want)
		}
	}
}

func TestNoteAuthNeedsAccountAndReason(t *testing.T) {
	var calls []markCall
	prev := markAuth
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth = prev })
	noteAuth(Dispatch{Account: ""}, &retry.Error{AuthReason: "r"})
	noteAuth(Dispatch{Account: "codex:x"}, &retry.Error{})
	noteAuth(Dispatch{Account: "codex:x"}, errors.New("plain"))
	if len(calls) != 0 {
		t.Fatalf("noteAuth marked without an account or reason: %+v", calls)
	}
	noteAuth(Dispatch{Account: "codex:x"}, &retry.Error{AuthReason: "r"})
	if len(calls) != 1 || calls[0].account != "codex:x" {
		t.Fatalf("calls = %+v", calls)
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
