package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An opencode record pinned to opencode-go (valid via PinIDs, no registry row) resolves to its own plan, not an error.
func TestPinnedProfileOpencodeGo(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	got, err := PinnedProfile("opencode")
	if err != nil || got != "" {
		t.Errorf("pinnedProfile(opencode) = %q, %v; want empty profile and no error", got, err)
	}
	vaulttest.SetRecord(t, "opencode", "profile", "no-such-profile")
	if _, err := PinnedProfile("opencode"); err == nil || !strings.Contains(err.Error(), `unknown profile "no-such-profile"`) {
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
	oe, os_ := EnsureRouter, RouterSecret
	t.Cleanup(func() { EnsureRouter, RouterSecret = oe, os_ })
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

// Inside databricks:dev a routed child holds a session token minted once for dev and this pid, never the router secret.
func TestArmRouterRouteMintsSessionToken(t *testing.T) {
	armRegistry(t, "")
	stubRouter(t, func(context.Context) (string, error) { return "http://127.0.0.1:9" + t.Name(), nil })
	om := MintSession
	t.Cleanup(func() { MintSession = om })
	var mints []string
	MintSession = func(_ context.Context, profile string, pid int) (string, error) {
		mints = append(mints, fmt.Sprintf("%s/%d", profile, pid))
		return "cct_ARM", nil
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, key, err := routerRoute(t.Context(), SchemaFor("codex"), reg.Providers["dev"], ""); err != nil || key != "cct_ARM" {
			t.Fatalf("armed routerRoute key = %q, %v; want the minted session token", key, err)
		}
	}
	if want := fmt.Sprintf("dev/%d", os.Getpid()); !slices.Equal(mints, []string{want}) {
		t.Errorf("mints = %v, want one for %s", mints, want)
	}
	t.Setenv(agentconfig.OverlayVar, "")
	if _, _, key, _ := routerRoute(t.Context(), SchemaFor("codex"), reg.Providers["dev"], ""); key != "rtr-secret-xyz" {
		t.Errorf("unarmed routerRoute key = %q, want the router secret", key)
	}
}

// A cached router token older than sessionTokenMaxAge is re-minted; a fresh one is reused.
func TestSessionTokenRemintsWhenStale(t *testing.T) {
	origMint := MintSession
	mints := 0
	MintSession = func(context.Context, string, int) (string, error) {
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
	MintSession = func(_ context.Context, profile string, _ int) (string, error) {
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

// TestCustomEnvLookupErrorLeavesExportedKeySurviving: a @secret ref that vault.Lookup cannot
// resolve (locked vault, no env fallback available to Lookup itself) must be left out of custom
// entirely, not merged in as K= — so an already-exported K carried in the base env list passes
// through Plan.Env untouched instead of being clobbered with an empty value.
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

// TestPlanVendorScrubsVaultKeyClaudeDoesNot: a vendor child (Plan.Vendor=true, set by launch.Vendor)
// loses HARNESS_VAULT_KEY from its inherited env; a plain (Claude Code) Plan keeps it, since the
// Claude Code launch itself still needs the key to open the vault for hooks.
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

// plainEnv scrubs the base environ before Set is layered on, so a target's own addition for a denied
// name (e.g. copilot's COPILOT_GITHUB_TOKEN) wins instead of being dropped.
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
