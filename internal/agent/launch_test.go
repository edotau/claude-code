package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
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
	armRegistry(t, "")
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
	t.Setenv(agentconfig.OverlayVar, "")
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
	armRegistry(t, "")
	t.Setenv(agentconfig.OverlayVar, "")
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
	e := sessionTokens[base+"|"+profile]
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
	custom := map[string]string{"ANTHROPIC_MODEL": "mine", "CLAUDE_X": "mine", "FOO": "bar", "PATH": "/custom"}
	env := p.envWith([]string{"PATH=/usr/bin", "KEEP=1", "CLAUDE_X=old"}, custom)
	for _, want := range []string{"ANTHROPIC_MODEL=harness", "FOO=bar", "PATH=/custom", "KEEP=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"ANTHROPIC_MODEL=mine", "CLAUDE_X=mine", "CLAUDE_X=old", "PATH=/usr/bin"} {
		if slices.Contains(env, bad) {
			t.Errorf("env has %q: %v", bad, env)
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
		t.Fatalf("Env = %v, want K=exported-value preserved from base", env)
	}
	if slices.Contains(env, "K=") {
		t.Fatalf("Env = %v, a Lookup error must not set K=", env)
	}
}

// A vendor child (Plan.Vendor) loses HARNESS_VAULT_KEY from its inherited env; a plain Claude Code Plan keeps it.
func TestPlanVendorScrubsVaultKeyClaudeDoesNot(t *testing.T) {
	base := []string{"HARNESS_VAULT_KEY=topsecret", "PATH=/usr/bin"}

	claude := Plan{}.envWith(base, nil)
	if !slices.Contains(claude, "HARNESS_VAULT_KEY=topsecret") {
		t.Errorf("Claude Plan.Env = %v, want HARNESS_VAULT_KEY kept", claude)
	}

	vendor := Plan{CLI: true}.envWith(base, nil)
	if slices.ContainsFunc(vendor, func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") }) {
		t.Errorf("vendor Plan.Env = %v, want HARNESS_VAULT_KEY scrubbed", vendor)
	}
	if !slices.Contains(vendor, "PATH=/usr/bin") {
		t.Errorf("vendor Plan.Env = %v, want PATH kept", vendor)
	}
}

// plainEnv scrubs the base environ before Set is layered on, so a target's addition for a denied name wins.
func TestPlanEnvTargetAdditionWinsOverDenyTable(t *testing.T) {
	p := Plan{Set: map[string]string{"COPILOT_GITHUB_TOKEN": "own-token"}}
	base := []string{"COPILOT_GITHUB_TOKEN=inherited", "PATH=/bin"}
	env := p.plainEnv(base)
	if !slices.Contains(env, "COPILOT_GITHUB_TOKEN=own-token") {
		t.Errorf("env = %q, want the plan's own COPILOT_GITHUB_TOKEN to win", env)
	}
	if slices.Contains(env, "COPILOT_GITHUB_TOKEN=inherited") {
		t.Errorf("env = %q, inherited value must not survive", env)
	}
}

// The scrub drops a denied name from base with no Set override; Keep names a survivor explicitly.
func TestPlanEnvScrubsDenyTableRespectingKeep(t *testing.T) {
	base := []string{"DATABRICKS_TOKEN=secret", "DATABRICKS_HOST=keepme", "PATH=/bin"}
	p := Plan{Keep: []string{"DATABRICKS_HOST"}}
	env := p.plainEnv(base)
	if slices.Contains(env, "DATABRICKS_TOKEN=secret") {
		t.Errorf("env = %q, DATABRICKS_TOKEN must be scrubbed", env)
	}
	if !slices.Contains(env, "DATABRICKS_HOST=keepme") {
		t.Errorf("env = %q, kept name must survive", env)
	}
	if !slices.Contains(env, "PATH=/bin") {
		t.Errorf("env = %q, non-credential var must survive", env)
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

// An AI Gateway-wire row is refused for every shared-path agent before any route is built; a serving row is not.
func TestConfigureRouteRefusesAIGatewayWire(t *testing.T) {
	vaulttest.Use(t)
	stubRouter(t, func(context.Context) (string, error) { return "", errors.New("router stubbed") })
	t.Setenv("DBX_GW_TEST_TOKEN", "tok")
	const doc = `{"providers":{"databricks":{"gw":{"preset":"databricks","host":"https://x.example.com",` +
		`"options":{"wire":"%s"},"auth":{"type":"key","env":"DBX_GW_TEST_TOKEN"}}}}}`
	const refusal = "profile gw uses the AI Gateway wire; agents run on /serving-endpoints only"
	for _, wire := range []string{providers.WireAIGateway, "serving"} {
		reg, err := providers.Parse([]byte("{}"), fmt.Appendf(nil, doc, wire))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"codex", "copilot", "gemini", "opencode"} {
			_, _, err := SchemaFor(name).configureRoute(t.Context(), providers.Target{Provider: reg.Providers["gw"], Model: "m"}, nil)
			if refused := err != nil && err.Error() == refusal; refused != (wire == providers.WireAIGateway) {
				t.Errorf("%s on %s: err = %v; refused = %v", name, wire, err, refused)
			}
		}
	}
}

// ApplyOverlay reads the run's overlay from req.Values, never from the parent's own environment.
func TestApplyOverlayIgnoresAmbientOverlay(t *testing.T) {
	t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"dev"}`)
	if got := (RunOptions{Values: proc.Values{"PST_RUN": "x"}}).ApplyOverlay(t.Context(), agentconfig.Resolved{}); len(got.Unset) != 0 {
		t.Errorf("ambient $%s reached a run whose Values do not name it: Unset = %v", agentconfig.OverlayVar, got.Unset)
	}
	named := RunOptions{Values: proc.Values{agentconfig.OverlayVar: `{"provider":"databricks"}`}}
	if got := named.ApplyOverlay(t.Context(), agentconfig.Resolved{}); !slices.Contains(got.Unset, agentconfig.OverlayVar) {
		t.Errorf("a run whose Values name the overlay must unset it for the child; Unset = %v", got.Unset)
	}
}

// With nil Values, ApplyOverlay reads the overlay from ctx's snapshot, not the parent's environment.
func TestApplyOverlayReadsCtxSnapshot(t *testing.T) {
	t.Setenv(agentconfig.OverlayVar, "")
	ctx := proc.WithValues(t.Context(), proc.Values{agentconfig.OverlayVar: `{"provider":"databricks"}`})
	if got := (RunOptions{}).ApplyOverlay(ctx, agentconfig.Resolved{}); !slices.Contains(got.Unset, agentconfig.OverlayVar) {
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

// unsignedJWT is a JWT-shaped credential whose exp claim is left in the future.
func unsignedJWT(left time.Duration) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"none"}`) + "." + seg(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(left).Unix())) + ".sig"
}

// sealM2M stores an m2m row's client secret and its sealed access slot, as a prior mint leaves them.
func sealM2M(t *testing.T, p *providers.Provider, access string) {
	t.Helper()
	name := providers.AuthSecretName(p)
	sum := sha256.Sum256([]byte(providers.AuthHost(p) + "\x00" + p.Auth.ClientID + "\x00" + "s3cret"))
	slot, err := json.Marshal(map[string]string{"access": access, "bind": hex.EncodeToString(sum[:8])})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{name: "s3cret", name + "_ACCESS": string(slot)} {
		if err := vault.Store(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// A direct run is never refused up front, even when its deadline outlasts the sealed token's remaining life.
func TestConfigureWireDirectNeverRefusesOnTokenLife(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "direct-life-m2m", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://dbc-x.cloud.databricks.com/serving-endpoints/anthropic",
		Auth:    providers.Auth{Type: providers.AuthM2M, ClientID: "cid"}}
	sealM2M(t, p, unsignedJWT(30*time.Minute))
	ctx, cancel := context.WithTimeout(WithDirect(t.Context()), 45*time.Minute)
	defer cancel()
	spy := &wireSpy{}
	if _, _, err := (Schema{Name: "codex"}).configureWire(ctx, spy, providers.Target{Provider: p, Model: "m"}, nil); err != nil || !spy.wired {
		t.Fatalf("err=%v wired=%v, want a wired direct run", err, spy.wired)
	}
}
