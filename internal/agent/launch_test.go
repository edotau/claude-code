package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An opencode record pinned to opencode-go (valid via PinIDs, no registry row) resolves to its own plan, not an error.
func TestPinnedProfileOpencodeGo(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	got, err := PinnedProfile(t.Context(), "opencode")
	if err != nil || got != "" {
		t.Errorf("PinnedProfile(opencode) = %q, %v; want empty profile and no error", got, err)
	}
	vaulttest.SetRecord(t, "opencode", "profile", "no-such-profile")
	if _, err := PinnedProfile(t.Context(), "opencode"); err == nil || !strings.Contains(err.Error(), `unknown profile "no-such-profile"`) {
		t.Errorf("an invalid pin must still be an error with the ValidAgentPin cause, got %v", err)
	}
}

// An unreadable stored records blob still launches copilot with its built-in --allow-all, and says so.
func TestLaunchSpecCorruptStoreKeepsBuiltins(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{not json")
	var notice strings.Builder
	orig := NoticeOut
	NoticeOut = &notice
	t.Cleanup(func() { NoticeOut = orig })
	if _, argv := SchemaFor("copilot").LaunchSpec(); !slices.Equal(argv, []string{"--allow-all"}) {
		t.Errorf("argv = %v, want the built-in [--allow-all]", argv)
	}
	if !strings.Contains(notice.String(), "agent-records is corrupt") {
		t.Errorf("notice = %q, want the corrupt-store notice", notice.String())
	}
}

func stubRouter(t *testing.T, ensure func(context.Context) (string, error)) {
	t.Helper()
	oe := EnsureRouter
	t.Cleanup(func() { EnsureRouter = oe })
	if ensure != nil {
		EnsureRouter = ensure
	}
}

func TestEnsureRouterErrorSurfaces(t *testing.T) {
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("boom") })
	_, _, err := SchemaFor("codex").Configure(context.Background(), target(t, "openai", "gpt-5"))
	if err == nil || !strings.Contains(err.Error(), "router") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// A routed child holds one minted token per scope, never the router secret: armed {dev}, unarmed {dev, fallback qa}.
func TestRouterRouteMintsScopedSessionToken(t *testing.T) {
	armRegistry(t)
	row := `{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"DEV_TOKEN"}}`
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"fallback":["qa"],"providers":{"databricks":{"dev":`+row+`,"qa":`+row+`}}}`)
	stubRouter(t, func(context.Context) (string, error) { return "http://127.0.0.1:9" + t.Name(), nil })
	om := MintSession
	t.Cleanup(func() { MintSession = om; ResetSessionTokens() })
	var mints []string
	MintSession = func(_ context.Context, profiles []string, pid int) (string, error) {
		mints = append(mints, fmt.Sprintf("%s/%d", strings.Join(profiles, ","), pid))
		return "cct_" + strings.Join(profiles, "+"), nil
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, key, err := routerRoute(t.Context(), SchemaFor("codex"), reg.Providers["dev"], ""); err != nil || key != "cct_dev" {
			t.Fatalf("armed routerRoute key = %q, %v; want the minted session token", key, err)
		}
	}
	t.Setenv(route.OverlayVar, "")
	if _, _, key, err := routerRoute(t.Context(), SchemaFor("codex"), reg.Providers["dev"], ""); err != nil || key != "cct_dev+qa" {
		t.Errorf("unarmed routerRoute key = %q, %v; want the token for dev and its fallback qa", key, err)
	}
	pid := os.Getpid()
	if want := []string{fmt.Sprintf("dev/%d", pid), fmt.Sprintf("dev,qa/%d", pid)}; !slices.Equal(mints, want) {
		t.Errorf("mints = %v, want %v", mints, want)
	}
}

// A failed mint fails the route: the child is never handed the router secret instead.
func TestRouterRouteMintFailureAborts(t *testing.T) {
	armRegistry(t)
	t.Setenv(route.OverlayVar, "")
	stubRouter(t, func(context.Context) (string, error) { return "http://127.0.0.1:9" + t.Name(), nil })
	om := MintSession
	t.Cleanup(func() { MintSession = om; ResetSessionTokens() })
	MintSession = func(context.Context, []string, int) (string, error) {
		return "", errors.New("too many live session tokens")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, _, key, err := routerRoute(t.Context(), SchemaFor("codex"), reg.Providers["dev"], "")
	if err == nil || key != "" || !strings.Contains(err.Error(), "too many live session tokens") {
		t.Fatalf("routerRoute = %q, %v; want the mint error and no key", key, err)
	}
}

// A /d/<id> tag on the router base never splits the token slot: two dispatches share one mint.
func TestSessionTokenIgnoresDispatchTag(t *testing.T) {
	origMint := MintSession
	mints := 0
	MintSession = func(context.Context, []string, int) (string, error) { mints++; return "tok", nil }
	t.Cleanup(func() { MintSession = origMint; ResetSessionTokens() })
	for _, base := range []string{"http://router.test-tag", "http://router.test-tag/d/a-1", "http://router.test-tag/d/b-2"} {
		if _, err := SessionToken(t.Context(), base, "p"); err != nil {
			t.Fatal(err)
		}
	}
	if mints != 1 {
		t.Errorf("mints = %d, want 1 across dispatch-tagged bases", mints)
	}
}

// A cached router token older than sessionTokenMaxAge is re-minted; a fresh one is reused.
func TestSessionTokenRemintsWhenStale(t *testing.T) {
	origMint := MintSession
	mints := 0
	MintSession = func(context.Context, []string, int) (string, error) {
		mints++
		return "tok" + strconv.Itoa(mints), nil
	}
	t.Cleanup(func() { MintSession = origMint })

	const base, profile = "http://router.test-stale", "p"
	ctx := context.Background()
	first, err := SessionToken(ctx, base, profile)
	if err != nil || first != "tok1" {
		t.Fatalf("first mint = %q, %v", first, err)
	}
	if again, _ := SessionToken(ctx, base, profile); again != "tok1" || mints != 1 {
		t.Fatalf("fresh token not reused: %q after %d mints", again, mints)
	}
	sessionMu.Lock()
	e := sessionTokens[base+"|"+profile+"|open"]
	sessionMu.Unlock()
	e.mu.Lock()
	e.minted = time.Now().Add(-2 * sessionTokenMaxAge)
	e.mu.Unlock()
	got, err := SessionToken(ctx, base, profile)
	if err != nil || got != "tok2" {
		t.Fatalf("stale token not re-minted: %q, %v", got, err)
	}
}

// A mint held for one profile never blocks a cached lookup for another.
func TestSessionTokenSlowMintBlocksOnlyItsProfile(t *testing.T) {
	origMint := MintSession
	t.Cleanup(func() { MintSession = origMint })
	entered, release := make(chan struct{}), make(chan struct{})
	MintSession = func(_ context.Context, profiles []string, _ int) (string, error) {
		profile := profiles[0]
		if profile == "slow" {
			close(entered)
			<-release
		}
		return "tok-" + profile, nil
	}
	base := "http://router." + t.Name()
	if _, err := SessionToken(t.Context(), base, "fast"); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = SessionToken(context.Background(), base, "slow") }()
	defer close(release)
	<-entered
	got := make(chan string, 1)
	go func() { tok, _ := SessionToken(context.Background(), base, "fast"); got <- tok }()
	select {
	case tok := <-got:
		if tok != "tok-fast" {
			t.Errorf("cached lookup = %q, want tok-fast", tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a held mint for profile slow blocked the cached lookup for profile fast")
	}
}

// D4: gemini (no Dialects) is refused on any non-login row, even one with a gemini route.
func TestGeminiRefusedOnRoutedRow(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "gem-key", Models: map[string]string{"opus": "gemini-x"},
		Routes: map[string]string{providers.ProtocolGemini: "https://g.example.com"}}
	if CanServe("gemini", p) {
		t.Error("CanServe(gemini) = true on a keyed row with a gemini route")
	}
	if !CanServe("opencode", p) {
		t.Error("control: opencode speaks gemini and should serve the row")
	}
	_, _, err := SchemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}

// configureRoute surfaces Serves' reason verbatim, prefixed by the agent.
func TestConfigureRouteUsesServesReason(t *testing.T) {
	vaulttest.Use(t)
	ant := &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}
	cases := []struct {
		agent string
		p     *providers.Provider
	}{
		{"gemini", ant},
		{"codex", ant},
		{"copilot", &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}},
	}
	for _, c := range cases {
		_, why := Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := SchemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}

// TestEnvWithCustomLosesToSetAndUnset: a custom var overrides the inherited env but never a launch Set or Unset key.
func TestEnvWithCustomLosesToSetAndUnset(t *testing.T) {
	p := Plan{Set: map[string]string{"ANTHROPIC_MODEL": "harness"}, Unset: []string{"CLAUDE_X"}}
	custom := map[string]string{"ANTHROPIC_MODEL": "mine", "CLAUDE_X": "mine", "FOO": "bar", "PATH": "/custom", "CODEMODE": "on"}
	env := p.envWith([]string{"PATH=/usr/bin", "KEEP=1", "CLAUDE_X=old"}, custom)
	for _, want := range []string{"ANTHROPIC_MODEL=harness", "FOO=bar", "PATH=/custom", "KEEP=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %d", want, len(env))
		}
	}
	for _, bad := range []string{"ANTHROPIC_MODEL=mine", "CLAUDE_X=mine", "CLAUDE_X=old", "PATH=/usr/bin", "CODEMODE=on"} {
		if slices.Contains(env, bad) {
			t.Errorf("env has %q: %d", bad, len(env))
		}
	}
}

// A @secret ref vault.Lookup cannot resolve is left out of custom, not merged as K=, so an exported K survives.
func TestCustomEnvLookupErrorLeavesExportedKeySurviving(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "K", "vault-value")
	vaulttest.Put(t, vault.Env, "K", providers.SecretRef)
	kr.Down = true // keyring unreachable; no HARNESS_VAULT_KEY and no K in the process env, so Lookup must error

	p := Plan{}
	env := p.Env([]string{"K=exported-value", "PATH=/usr/bin"})
	if !slices.Contains(env, "K=exported-value") {
		t.Fatalf("Env = %d, want K=exported-value preserved from base", len(env))
	}
	if slices.Contains(env, "K=") {
		t.Fatalf("Env = %d, a Lookup error must not set K=", len(env))
	}
}

// A vendor child (Plan.Vendor) loses HARNESS_VAULT_KEY from its inherited env; a plain Claude Code Plan keeps it.
func TestPlanVendorScrubsVaultKeyClaudeDoesNot(t *testing.T) {
	base := []string{"HARNESS_VAULT_KEY=topsecret", "PATH=/usr/bin"}

	claude := Plan{}.envWith(base, nil)
	if !slices.Contains(claude, "HARNESS_VAULT_KEY=topsecret") {
		t.Errorf("Claude Plan.Env = %d, want HARNESS_VAULT_KEY kept", len(claude))
	}

	vendor := Plan{CLI: true}.envWith(base, nil)
	if slices.ContainsFunc(vendor, func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") }) {
		t.Errorf("vendor Plan.Env = %d, want HARNESS_VAULT_KEY scrubbed", len(vendor))
	}
	if !slices.Contains(vendor, "PATH=/usr/bin") {
		t.Errorf("vendor Plan.Env = %d, want PATH kept", len(vendor))
	}
}

// A vendor launch keeps its own agent's login names unless ScrubDeny blocks them, and drops every other agent's.
func TestPlanVendorKeepsOwnLogin(t *testing.T) {
	foreign := []string{"MISTRAL_API_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN"}
	for name, own := range proc.OwnLoginEnv {
		if name == "claude" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			names := slices.Concat(proc.ForeignLogins("", false), foreign)
			var base []string
			for _, n := range names {
				t.Setenv(n, "dummy")
				base = append(base, n+"=dummy")
			}
			p := Plan{CLI: true, Agent: name}
			env, unset := p.envWith(base, nil), p.unsetNames()
			for _, n := range names {
				want := slices.Contains(own, n) && !proc.EnvBlocked(n)
				if got := slices.Contains(env, n+"=dummy"); got != want {
					t.Errorf("%s: %s kept = %v, want %v", name, n, got, want)
				}
				if slices.Contains(unset, n) == want {
					t.Errorf("%s: %s listed under unset = %v, want %v", name, n, want, !want)
				}
			}
		})
	}
}

// An interactive vendor plan's Set credential beats a different inherited value of the same name.
func TestPlanInjectedCredentialBeatsInherited(t *testing.T) {
	for _, r := range injectedRows {
		t.Run(r.agent+"/"+r.name, func(t *testing.T) {
			p := Plan{CLI: true, Agent: r.agent, Set: map[string]string{r.name: "injected"}}
			if n, val := injectedOnce(p.plainEnv([]string{"PATH=/bin", r.name + "=inherited"}), r.name); n != 1 || val != "injected" {
				t.Errorf("%s: %d entries, value %q; want one entry, injected", r.name, n, val)
			}
		})
	}
}

// plainEnv scrubs the base environ before Set is layered on, so a target's addition for a denied name wins.
func TestPlanEnvTargetAdditionWinsOverDenyTable(t *testing.T) {
	p := Plan{Set: map[string]string{"COPILOT_GITHUB_TOKEN": "own-token"}}
	base := []string{"COPILOT_GITHUB_TOKEN=inherited", "PATH=/bin"}
	env := p.plainEnv(base)
	if !slices.Contains(env, "COPILOT_GITHUB_TOKEN=own-token") {
		t.Errorf("env = %d, want the plan's own COPILOT_GITHUB_TOKEN to win", len(env))
	}
	if slices.Contains(env, "COPILOT_GITHUB_TOKEN=inherited") {
		t.Errorf("env = %d, inherited value must not survive", len(env))
	}
}

// The scrub drops a denied name from base with no Set override; Keep names a survivor explicitly.
func TestPlanEnvScrubsDenyTableRespectingKeep(t *testing.T) {
	base := []string{"DATABRICKS_TOKEN=secret", "DATABRICKS_HOST=keepme", "PATH=/bin"}
	p := Plan{Keep: []string{"DATABRICKS_HOST"}}
	env := p.plainEnv(base)
	if slices.Contains(env, "DATABRICKS_TOKEN=secret") {
		t.Errorf("env = %d, DATABRICKS_TOKEN must be scrubbed", len(env))
	}
	if !slices.Contains(env, "DATABRICKS_HOST=keepme") {
		t.Errorf("env = %d, kept name must survive", len(env))
	}
	if !slices.Contains(env, "PATH=/bin") {
		t.Errorf("env = %d, non-credential var must survive", len(env))
	}
}

// Print lists only dropped names (never values), and never a name the plan itself sets.
func TestPlanPrintListsUnsetNamesOnly(t *testing.T) {
	t.Setenv("DATABRICKS_TOKEN", "super-secret-value")
	t.Setenv("DATABRICKS_HOST", "super-secret-value")
	p := Plan{Binary: "/bin/true", Set: map[string]string{"DATABRICKS_HOST": "own"}}
	var buf bytes.Buffer
	p.Print(&buf)
	out := buf.String()
	if !strings.Contains(out, "unset    DATABRICKS_TOKEN\n") {
		t.Errorf("output %q, want an unset line for DATABRICKS_TOKEN", out)
	}
	if strings.Contains(out, "unset    DATABRICKS_HOST") {
		t.Errorf("output %q, DATABRICKS_HOST is set by the plan, must not be listed as unset", out)
	}
	if strings.Contains(out, "super-secret-value") {
		t.Errorf("output %q leaked a value", out)
	}
}

func TestPrintRedactsCredentialSegmentNames(t *testing.T) {
	set := map[string]string{"DATA_PATH": "/visible"}
	for _, k := range []string{"FOO_HEADERS", "MY_AUTH", "DB_CREDENTIALS", "DATABRICKS_PAT", "X_BEARER", "X_AUTH_Y"} {
		set[k] = "s3cr3t-value"
	}
	var b bytes.Buffer
	(&Plan{Set: set}).Print(&b)
	out := b.String()
	if strings.Contains(out, "s3cr3t-value") {
		t.Errorf("Print leaked a credential-shaped value:\n%s", out)
	}
	if !strings.Contains(out, "DATA_PATH=/visible") {
		t.Errorf("benign DATA_PATH was redacted:\n%s", out)
	}
}

// ApplyOverlay reads the run's overlay from req.Values, never from the parent's own environment.
func TestApplyOverlayIgnoresAmbientOverlay(t *testing.T) {
	t.Setenv(route.OverlayVar, `{"provider":"databricks","profile":"dev"}`)
	if got := (RunOptions{Values: proc.Values{"PST_RUN": "x"}}).ApplyOverlay(t.Context(), agentconfig.Resolved{}); !slices.Equal(got.Unset, []string{route.OverlayVar}) {
		t.Errorf("ambient $%s must be unset for a run whose Values do not name it: Unset = %v", route.OverlayVar, got.Unset)
	}
	named := RunOptions{Values: proc.Values{route.OverlayVar: `{"provider":"databricks"}`}}
	if got := named.ApplyOverlay(t.Context(), agentconfig.Resolved{}); !slices.Contains(got.Unset, route.OverlayVar) {
		t.Errorf("a run whose Values name the overlay must unset it for the child; Unset = %v", got.Unset)
	}
}

// With nil Values, ApplyOverlay reads the overlay from ctx's snapshot, not the parent's environment.
func TestApplyOverlayReadsCtxSnapshot(t *testing.T) {
	t.Setenv(route.OverlayVar, "")
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"provider":"databricks"}`})
	if got := (RunOptions{}).ApplyOverlay(ctx, agentconfig.Resolved{}); !slices.Contains(got.Unset, route.OverlayVar) {
		t.Errorf("nil Values must read the overlay from ctx's snapshot; Unset = %v", got.Unset)
	}
}

// wireSpy is a Driver whose Wire records that configureWire got that far.
type wireSpy struct {
	Driver
	wired bool
}

func (w *wireSpy) Wire(context.Context, WireRoute, *Headless) ([]string, []string, error) {
	w.wired = true
	return nil, nil, nil
}

// A direct run is never refused up front, even when its deadline outlasts the token's recorded remaining life.
func TestConfigureWireDirectNeverRefusesOnTokenLife(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "direct-life-m2m", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://dbc-x.cloud.databricks.com/serving-endpoints/anthropic",
		Auth:    providers.Auth{Type: providers.AuthM2M, ClientID: "cid"}}
	writeExpiryRecord(t, p, time.Now().Add(30*time.Minute))
	orig := credential
	credential = func(context.Context, *providers.Provider) (string, error) { return "fixture-access", nil }
	t.Cleanup(func() { credential = orig })
	ctx, cancel := context.WithTimeout(WithDirect(t.Context()), 45*time.Minute)
	defer cancel()
	spy := &wireSpy{}
	if _, _, err := (Schema{Name: "codex"}).configureWire(ctx, spy, providers.Target{Provider: p, Model: "m"}, nil); err != nil || !spy.wired {
		t.Fatalf("err=%v wired=%v, want a wired direct run", err, spy.wired)
	}
}

func TestConfinementCarriesDispatchLimits(t *testing.T) {
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"maxChildren":0,"maxTreeTokens":123,"budgetUSD":1}`})
	for range 2 {
		doc := confinementDocContext(ctx)
		o, err := agentconfig.LoadOverlayContext(t.Context(), doc, nil)
		if err != nil || o.MaxChildren == nil || *o.MaxChildren != 0 || o.MaxTreeTokens == nil || *o.MaxTreeTokens != 123 || o.BudgetUSD != nil {
			t.Fatalf("nested limits: %s, %v", doc, err)
		}
		ctx = proc.WithValues(t.Context(), proc.Values{route.OverlayVar: doc})
	}
}

// Given a budget-only overlay, an interactive launch is refused naming the budget keys; a limit-only one passes.
func TestLaunchOverlayRefusalNamesKeys(t *testing.T) {
	for _, c := range []struct{ doc, want string }{
		{`{"budgetUSD":1}`, "budgetUSD/budgetCredits"},
		{`{"maxChildren":2}`, ""},
	} {
		_, err := launchOverlay(LaunchOptions{Overlay: c.doc, PrintEnv: true})
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err %v, want %q", c.doc, err, c.want)
		}
	}
}

// A rejection while this process holds a token the proxies refuse is the proxy's own: no account mark, one re-mint.
func TestProxySessionRefusalNeverMarksAndRemintsOnce(t *testing.T) {
	var calls []markCall
	prevMark, prevInvalid := markAuth, sessionInvalid
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth, sessionInvalid = prevMark, prevInvalid; ResetSessionTokens() })
	ResetSessionTokens()
	sessionTokens["base|dev"] = &sessionToken{tok: "cct_stale", minted: time.Now()}
	sessionInvalid = func(string, string) error { return errors.New("signed with another secret") }
	rejected := &retry.Error{AuthReason: "login rejected"}
	noteAuth(Dispatch{Account: "databricks·dev"}, rejected)
	if len(calls) != 0 {
		t.Fatalf("a proxy refusal marked the account: %+v", calls)
	}
	refreshed := 0
	rec := recoverSession(func(context.Context) error { refreshed++; return nil })
	if err := rec(context.Background()); err != nil || sessionTokens["base|dev"].tok != "" || refreshed != 0 {
		t.Fatalf("first recover = %v, provider refreshed %d", err, refreshed)
	}
	sessionTokens["base|dev"].tok = "cct_stale" // the re-minted token is refused too: the next recover is the provider's
	if err := rec(context.Background()); err != nil || refreshed != 1 {
		t.Fatalf("second recover = %v, provider refreshed %d, want 1", err, refreshed)
	}
	sessionInvalid = func(string, string) error { return nil } // the proxy accepts the token: the rejection is upstream's
	noteAuth(Dispatch{Account: "databricks·dev"}, rejected)
	if len(calls) != 1 {
		t.Fatalf("an upstream rejection did not mark: %+v", calls)
	}
	sessionInvalid = func(string, string) error { return proxy.ErrSessionUnverified } // unjudgeable: never a mark
	noteAuth(Dispatch{Account: "databricks·dev"}, rejected)
	if len(calls) != 1 {
		t.Fatalf("an unverifiable token let a mark through: %+v", calls)
	}
}

// An attempt that saw the proxy's own refusal records it as a typed field, and that field alone stops the mark.
func TestProxyRefusalRecordedOnTheAttempt(t *testing.T) {
	var calls []markCall
	prevMark := markAuth
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth = prevMark })
	attempt := &retry.Error{Kind: retry.KindAuth, AuthReason: "login rejected", Err: errors.New("401")}
	markProxyRefusal(context.Background(), attempt, errors.New("401 "+proxy.RefusalSession))
	if noteAuth(Dispatch{Account: "databricks·dev"}, attempt); !attempt.ProxyRefused || len(calls) != 0 {
		t.Fatalf("ProxyRefused %v, marks %+v", attempt.ProxyRefused, calls)
	}
}

func TestEndpointURL(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "")
	routed := false
	stubRouter(t, func(context.Context) (string, error) { routed = true; return "http://127.0.0.1:9", nil })
	keyed := &providers.Provider{Name: "qa", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://qa.cloud.databricks.com/serving-endpoints/anthropic", Auth: providers.Auth{Type: providers.AuthPAT}}
	local := &providers.Provider{Name: "loc", Provider: "openai", Protocol: providers.ProtocolOpenAI, BaseURL: "http://localhost:11434/v1",
		Auth: providers.Auth{Type: providers.AuthNone}}
	dry := context.WithValue(t.Context(), launchDryKey{}, true)
	for _, tc := range []struct {
		name   string
		p      *providers.Provider
		r      route.Route
		base   string
		routed bool
		err    string
	}{
		{"missing endpoint on a keyed row is the proxy", keyed, route.Route{}, "http://127.0.0.1:9/p/qa", true, ""},
		{"proxy", keyed, route.Route{Endpoint: "proxy"}, "http://127.0.0.1:9/p/qa", true, ""},
		{"direct is the row's own", keyed, route.Route{Endpoint: "direct"}, keyed.BaseURL, false, ""},
		{"a row with no key has no proxy", local, route.Route{Endpoint: "proxy"}, local.BaseURL, false, ""},
		{"a team label on a row with no key is direct", local, route.Route{Endpoint: "phase:g1"}, local.BaseURL, false, ""},
		{"a URL with credentials", keyed, route.Route{Endpoint: "https://user@qa.cloud.databricks.com/x"}, "", false, "no credentials"},
		{"a URL with a query", keyed, route.Route{Endpoint: "https://qa.cloud.databricks.com/x?k=v"}, "", false, "no credentials"},
		{"the router URL on a row with no key is refused", local, route.Route{Endpoint: proxy.Base(proxy.DefaultPort)}, "", false, "not the loc row's own"},
		{"the router URL under another provider's unit", keyed, route.Route{Endpoint: proxy.Base(proxy.DefaultPort), Provider: "openai", Profile: "loc"}, "", false, "names"},
		{"a URL on the row's host", keyed, route.Route{Endpoint: "https://qa.cloud.databricks.com/x/", Provider: "databricks", Profile: "qa"},
			"https://qa.cloud.databricks.com/x", false, ""},
		{"a URL off the row", keyed, route.Route{Endpoint: "https://evil.example/x"}, "", false, "not the qa row's own"},
		{"a URL for another profile", keyed, route.Route{Endpoint: "https://qa.cloud.databricks.com/x", Provider: "databricks", Profile: "dev"}, "", false, "names"},
		{"the router's own loopback URL is the proxy", keyed, route.Route{Endpoint: proxy.Base(proxy.DefaultPort)}, "", true, ""},
		{"a loopback URL that is not the router", keyed, route.Route{Endpoint: "http://127.0.0.1:1"}, "", false, "not the qa row's own"},
	} {
		routed = false
		ep, err := EndpointURL(dry, tc.p, tc.r, tc.p.Protocol)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) || routed {
				t.Errorf("%s: err = %v routed=%v, want %q before any router", tc.name, err, routed, tc.err)
			}
		case err != nil || ep.Routed != tc.routed || tc.routed && !strings.HasSuffix(ep.Base, "/p/qa") || !tc.routed && ep.Base != tc.base:
			t.Errorf("%s: %+v, %v; want base %q routed %v", tc.name, ep, err, tc.base, tc.routed)
		}
	}
}

// A failover mint is held to the plan's fence: a candidate outside it is refused before any mint.
func TestFailoverMintRefusesProfileOutsidePlanFence(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "")
	om := MintSessionWithFailover
	t.Cleanup(func() { MintSessionWithFailover = om; ResetSessionTokens() })
	var minted []string
	MintSessionWithFailover = func(_ context.Context, profiles []string, _ int, _ string) (string, error) {
		minted = append(minted, strings.Join(profiles, ","))
		return "cct_" + strings.Join(profiles, "+"), nil
	}
	ctx := withPlan(t.Context(), RoutePlan{Fence: route.Fence{Profiles: []string{"qa"}}})
	const base = "https://router.example"
	if _, err := SessionTokenWithFailover(ctx, base, "databricks", "prod"); err == nil || !strings.Contains(err.Error(), "outside the plan fence") {
		t.Fatalf("prod under a [qa] fence: err = %v, want a refusal", err)
	}
	if tok, err := SessionTokenWithFailover(ctx, base, "databricks", "qa"); err != nil || tok != "cct_qa" {
		t.Fatalf("qa under a [qa] fence: %q, %v", tok, err)
	}
	if !slices.Equal(minted, []string{"qa"}) {
		t.Errorf("minted = %v, want only qa", minted)
	}
}

// A plan with a zero-value fence mints under the overlay's confinement, not a refusal.
func TestMintFenceZeroPlanFenceFallsBack(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "")
	want, err := proxy.ConfinementFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, err := mintFence(withPlan(t.Context(), RoutePlan{}))
	if err != nil || got.Open != want.Open || !slices.Equal(got.Profiles, want.Profiles) {
		t.Fatalf("mintFence = %+v, %v; want %+v", got, err, want)
	}
	if got, err := mintFence(withPlan(t.Context(), RoutePlan{Fenced: true})); err != nil || got.Open || len(got.Profiles) > 0 {
		t.Fatalf("a decided deny-all fence = %+v, %v; want closed and empty", got, err)
	}
}

// A bad overlay fails RunUnit with an error, and the child env slice is not mutated by withChosenDoc.
func TestRunUnitSurfacesOverlayError(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "{not json")
	if _, err := RunUnit(t.Context()); err == nil {
		t.Fatal("RunUnit swallowed a bad overlay")
	}
	if IsDirect(t.Context()) {
		t.Fatal("a bad overlay must not read as direct")
	}
}

func TestWithChosenDocLeavesCallerEnv(t *testing.T) {
	vaulttest.Use(t)
	const id = "wcd1"
	routeMu.Lock()
	chosenByID[id] = "phase:g1"
	routeMu.Unlock()
	t.Cleanup(func() { routeMu.Lock(); delete(chosenByID, id); routeMu.Unlock() })
	ctx := context.WithValue(t.Context(), dispatchIDKey{}, id)
	doc := `{"provider":"databricks","profile":"qa"}`
	env := []string{route.OverlayVar + "=" + doc}
	out := withChosenDoc(ctx, env)
	if env[0] != route.OverlayVar+"="+doc {
		t.Fatalf("caller env mutated: %q", env[0])
	}
	if !strings.Contains(out[0], "phase:g1") {
		t.Fatalf("chosen team not carried: %q", out[0])
	}
}

// A gemini launch with no typed model takes its own default silently; a typed model is spelled as typed.
func TestCliTargetGeminiDefaultIsSilent(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("GEMINI_MODEL", "")
	const opus = "databricks-claude-opus-5-5"
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, Host: "https://dbx.example.com",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_GEMINI_DEFAULT_TEST"}}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dbx": p}}
	sel := providers.Selection{Provider: p, Aliases: map[string]providers.Target{
		models.Opus: {Provider: p, Model: opus, Pinned: true}}}
	want := router.DatabricksModel("gemini", models.Opus, p)
	for _, c := range []struct{ name, typed, model string }{
		{"no typed model", "", want},
		{"typed claude model", "claude-opus-4-1", "claude-opus-4-1"},
	} {
		got, note, err := cliTarget(t.Context(), SchemaFor("gemini"), "gemini", reg, sel, true, c.typed)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Model != c.model || note != "" {
			t.Errorf("%s: model=%q note=%q, want model %q and no note", c.name, got.Model, note, c.model)
		}
		if got.Model == opus {
			t.Errorf("%s: the session's Claude model leaked into the gemini launch", c.name)
		}
	}
}

// With the Windows case rule on, route-name guards, RouteEnvLast and the unset line match any spelling.
func TestRouteNameChecksFoldOnWindows(t *testing.T) {
	proc.FoldCaseForTest(t, true)
	route := []string{"ANTHROPIC_BASE_URL=http://x"}
	if err := CheckRouteNames(route, []string{"anthropic_base_url"}, nil); err == nil {
		t.Error("CheckRouteNames let a case-variant set through")
	}
	if err := CheckRouteNames(route, nil, []string{"Anthropic_Base_Url"}); err == nil {
		t.Error("CheckRouteNames let a case-variant unset through")
	}
	if got := RouteEnvLast([]string{"anthropic_base_url=old", "KEEP=1"}, route); !slices.Equal(got, []string{"KEEP=1", route[0]}) {
		t.Errorf("RouteEnvLast = %q, want the case variant dropped", got)
	}
	p := Plan{Set: map[string]string{"ANTHROPIC_BASE_URL": "http://x"}}
	if err := p.checkOverlay(agentconfig.Resolved{Set: map[string]string{"anthropic_base_url": "y"}}); err == nil {
		t.Error("checkOverlay let a case-variant set through")
	}
	got := p.withOverlay(agentconfig.Resolved{Set: map[string]string{"anthropic_base_url": "y"}}).Set
	spellings := slices.DeleteFunc(slices.Collect(maps.Keys(got)), func(k string) bool { return !strings.EqualFold(k, "ANTHROPIC_BASE_URL") })
	if len(spellings) != 1 || got["ANTHROPIC_BASE_URL"] != "http://x" { // the confinement document is another name, not a spelling
		t.Errorf("withOverlay Set = %v, want one spelling", got)
	}
	t.Setenv("Databricks_Token", "v")
	var buf bytes.Buffer
	Plan{Unset: []string{"DATABRICKS_TOKEN"}}.Print(&buf)
	if !strings.Contains(buf.String(), "unset    DATABRICKS_TOKEN\n") {
		t.Errorf("Print = %q, want an unset line for the case-variant inherited name", buf.String())
	}
}

// Inside a confinement a launch's own unit rides by value to its child; an unconfined launch never invents a fence.
func TestPlanChildOverlayCarriesLaunchUnit(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	qa := map[string]string{providers.EnvSessionProfile: "qa"}
	fenced := proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"qa","fence":{"profiles":["qa"]}}`}
	scoped := route.Route{Provider: "databricks", Profile: "qa", Endpoint: "proxy", Identity: "sp"}
	for name, tc := range map[string]struct {
		plan Plan
		want route.Route
	}{
		"unconfined launch": {Plan{Set: qa, Unit: scoped}, route.Route{}},
		"confined launch":   {Plan{Set: qa, Values: fenced, Unit: scoped}, scoped},
		"auto pick": {Plan{Set: qa, Values: fenced, Unit: route.Route{Provider: "databricks", Profile: route.AutoArm, Endpoint: "proxy", Identity: "sp"}},
			scoped},
		"auto pick drops a row URL": {Plan{Set: qa, Values: fenced,
			Unit: route.Route{Provider: "databricks", Profile: route.AutoArm, Endpoint: "https://dev.cloud.databricks.com/x", Identity: "sp"}},
			route.Route{Provider: "databricks", Profile: "qa", Identity: "sp"}},
		"profile only": {Plan{Set: qa, Values: fenced, Unit: route.Route{Profile: "qa", Identity: "user"}},
			route.Route{Provider: "databricks", Profile: "qa", Identity: "user"}},
		"other provider": {Plan{Set: qa, Values: fenced, Unit: route.Route{Provider: "github", Account: "aueric_boehcp"}},
			route.Route{Provider: "databricks", Profile: "qa"}},
		"another row": {Plan{Set: map[string]string{providers.EnvSessionProfile: "dev"}, Values: fenced,
			Unit: route.Route{Provider: "databricks", Profile: "dev", Endpoint: "proxy"}}, route.Route{Provider: "databricks", Profile: "qa"}},
		"no launch unit": {Plan{Set: qa, Values: fenced}, route.Route{Provider: "databricks", Profile: "qa"}},
	} {
		t.Run(name, func(t *testing.T) {
			doc := tc.plan.withOverlay(agentconfig.Resolved{}).Set[route.OverlayVar]
			var o agentconfig.Overlay
			if err := json.Unmarshal([]byte(doc), &o); err != nil || o.Route() != tc.want {
				t.Fatalf("child doc %q (%v), want unit %+v", doc, err, tc.want)
			}
			if fence := tc.plan.Values[route.OverlayVar] != ""; fence != (o.Fence != nil) || fence && !slices.Equal(o.Fence.Profiles, []string{"qa"}) {
				t.Fatalf("child fence = %+v, want the inherited one only", o.Fence)
			}
			if _, err := launchOverlay(LaunchOptions{Values: proc.Values{route.OverlayVar: doc}}); err != nil {
				t.Fatalf("a nested launch refused its inherited unit: %v", err)
			}
		})
	}
	auto := proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"auto"}`}
	if _, err := launchOverlay(LaunchOptions{Values: auto}); err != nil {
		t.Fatalf("a nested launch refused an inherited auto arm: %v", err)
	}
	picked := Plan{Set: qa, Values: auto, Unit: route.Route{Provider: "databricks", Profile: "qa", Endpoint: "https://qa.cloud.databricks.com/x", Identity: "user"}}
	var o agentconfig.Overlay
	doc := picked.withOverlay(agentconfig.Resolved{}).Set[route.OverlayVar]
	if err := json.Unmarshal([]byte(doc), &o); err != nil || o.Route() != (route.Route{Provider: "databricks", Profile: route.AutoArm, Identity: "user"}) || o.Fence != nil {
		t.Fatalf("auto arm child doc %q (%v): want the arm kept with the launch identity and no row URL", doc, err)
	}
}
