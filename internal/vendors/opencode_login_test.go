package vendors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// isolateOpencodeRouteEnv clears every credential/login surface DescribeRoute's opencode/copilot chain
// reads, so a test controls precisely which leg (if any) resolves, never a real machine's own key.
func isolateOpencodeRouteEnv(t *testing.T) (dataHome, cfgHome string) {
	t.Helper()
	dataHome, cfgHome = t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())          // no real `gh`: CopilotLoginPresent's shell-out leg can't fire by accident
	t.Setenv("COPILOT_HOME", t.TempDir())  // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir()) // no real gh hosts.yml: GHActiveAccount reads nothing
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	return dataHome, cfgHome
}

// workableCopilotPin pins user as a gh login (hosts.yml in the isolated GH_CONFIG_DIR) with 10% of credits used.
func workableCopilotPin(t *testing.T, user string) {
	t.Helper()
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("GH_CONFIG_DIR"), "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	setPin(t, providers.PinCopilotProfile, user)
	prev := CopilotUsedPct
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	t.Cleanup(func() { CopilotUsedPct = prev })
}

func subscriptionSelection(t *testing.T, reg *providers.Registry) providers.Selection {
	t.Helper()
	sub, err := reg.Get("max")
	if err != nil {
		t.Fatal(err)
	}
	return providers.Selection{Provider: sub, Slots: map[string]providers.Target{"opus": {Provider: sub, Model: "claude-opus-5-5"}}}
}

// opencode's own `auth login` store for a NON-copilot provider (e.g. "anthropic") is no longer a
// fallback leg by itself: DescribeRoute's opencode chain is copilot-login -> anthropic-key -> none,
// so an opencode-stored "anthropic" login with no ANTHROPIC_API_KEY resolves to LegNone, never
// ErrOwnLogin — and the subscription session provider is never chosen either.
func TestOpencodeNonCopilotOwnLoginIsNotAFallbackLeg(t *testing.T) {
	data, _ := isolateOpencodeRouteEnv(t)
	setPin(t, providers.PinProfile, "max")
	if got := OpencodeLogins(); got != nil {
		t.Fatalf("no auth.json yet, got %v", got)
	}
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"anthropic":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OpencodeLogins(); len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("logins = %v", got)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel := subscriptionSelection(t, reg)
	target, _, err := ResolveTarget(reg, "opencode", sel, false)
	if errors.Is(err, ErrOwnLogin) {
		t.Fatal("a non-copilot opencode auth.json entry must not resolve to ErrOwnLogin")
	}
	if err == nil {
		t.Fatalf("want the LegNone error with no credential present; got target=%+v", target)
	}
	if !strings.Contains(err.Error(), "claude-code configure github") {
		t.Errorf("error = %q, want it to name `claude-code configure github`", err)
	}
	if target.Provider != nil && target.Provider.Auth.Type == "passthrough" {
		t.Errorf("the subscription (passthrough) provider must never be chosen for opencode; got %+v", target.Provider)
	}
	// --provider is explicit: the session provider is used as-is regardless of the fallback chain.
	if _, _, err := ResolveTarget(reg, "opencode", sel, true); err != nil {
		t.Errorf("an explicit provider must resolve as-is, got err=%v", err)
	}
}

// DescribeRoute's opencode chain in priority order: own-login (opencode Go/Zen), then copilot-login, then none —
// an ANTHROPIC_API_KEY alone is no route (the BYOK fallback is gone), and the subscription provider is never picked.
func TestDescribeOpenCodeRouteChainOrder(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}

	if ri := DescribeRoute(reg, "opencode"); ri.OK || ri.Leg != LegNone {
		t.Fatalf("with nothing configured, want LegNone; got %+v", ri)
	}

	t.Setenv("ANTHROPIC_API_KEY", secret)
	if ri := DescribeRoute(reg, "opencode"); ri.OK || ri.Leg != LegNone {
		t.Fatalf("an ANTHROPIC_API_KEY alone must not route opencode; got %+v", ri)
	}

	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ri := DescribeRoute(reg, "opencode"); !ri.OK || ri.Leg != LegCopilotLogin || ri.Provider != "personal" {
		t.Fatalf("opencode's own copilot login must route; got %+v", ri)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"),
		[]byte(`{"github-copilot":{"type":"oauth"},"opencode":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ri := DescribeRoute(reg, "opencode"); !ri.OK || ri.Leg != LegOwnLogin || ri.Provider != "opencode-go" {
		t.Fatalf("opencode's own opencode login must outrank the copilot login; got %+v", ri)
	}

	// ResolveTarget on the LegNone case returns exactly DescribeRoute's reason as the error.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	none := DescribeRoute(reg, "opencode")
	sel := subscriptionSelection(t, reg)
	_, _, rerr := ResolveTarget(reg, "opencode", sel, false)
	if rerr == nil || rerr.Error() != none.Reason {
		t.Fatalf("ResolveTarget error = %v, want DescribeRoute's reason %q", rerr, none.Reason)
	}
}

// Same chain order for copilot: copilot-login, then byok-anthropic, then none.
func TestDescribeCopilotRouteChainOrder(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}

	if ri := DescribeRoute(reg, "copilot"); ri.OK || ri.Leg != LegNone {
		t.Fatalf("with nothing configured, want LegNone; got %+v", ri)
	}

	t.Setenv("ANTHROPIC_API_KEY", secret)
	if ri := DescribeRoute(reg, "copilot"); !ri.OK || ri.Leg != LegByokAnthropic || ri.Provider != "anthropic" {
		t.Fatalf("with only ANTHROPIC_API_KEY, want byok-anthropic; got %+v", ri)
	}

	workableCopilotPin(t, "alice")
	if ri := DescribeRoute(reg, "copilot"); !ri.OK || ri.Leg != LegCopilotLogin || ri.Provider != "personal" {
		t.Fatalf("copilot's own GitHub login must outrank BYOK; got %+v", ri)
	}

	setPin(t, providers.PinCopilotProfile, "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	none := DescribeRoute(reg, "copilot")
	if none.OK || none.Leg != LegNone || !strings.Contains(none.Reason, "claude-code configure github") {
		t.Fatalf("LegNone's reason must name `claude-code configure github`; got %+v", none)
	}
	sel := subscriptionSelection(t, reg)
	_, _, rerr := ResolveTarget(reg, "copilot", sel, false)
	if rerr == nil || rerr.Error() != none.Reason {
		t.Fatalf("ResolveTarget error = %v, want DescribeRoute's reason %q", rerr, none.Reason)
	}
}

// On the copilot-login leg, DescribeRoute's Model comes from the shared resolver (providers.CopilotModel):
// the COPILOT_MODEL pin when set, else github-copilot's own opus slot — never the registry's stale value.
func TestDescribeCopilotRouteModelFollowsPin(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	workableCopilotPin(t, "alice")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}

	wantDefault := reg.Providers["personal"].Models["opus"]
	if ri := DescribeRoute(reg, "copilot"); ri.Model != wantDefault {
		t.Fatalf("Model (unpinned) = %q, want github-copilot's own opus slot %q", ri.Model, wantDefault)
	}

	if err := providers.SetPins(map[string]string{providers.PinCopilotModel: "claude-opus-5.5"}); err != nil {
		t.Fatal(err)
	}
	if ri := DescribeRoute(reg, "copilot"); ri.Model != "claude-opus-5.5" {
		t.Fatalf("Model (pinned) = %q, want the COPILOT_MODEL pin claude-opus-5.5", ri.Model)
	}
}

// --model (explicit=true) bypasses the fallback chain entirely: ResolveTarget returns the session
// slot's model unchanged, even when a COPILOT_MODEL pin names something else.
func TestResolveTargetCopilotExplicitModelBeatsPin(t *testing.T) {
	isolateOpencodeRouteEnv(t)
	t.Setenv("GH_TOKEN", secret)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := providers.SetPins(map[string]string{providers.PinCopilotModel: "claude-opus-5.5"}); err != nil {
		t.Fatal(err)
	}
	sub := subscriptionSelection(t, reg)
	sub.Slots["opus"] = providers.Target{Provider: sub.Provider, Model: "explicit-model"}

	target, note, err := ResolveTarget(reg, "copilot", sub, true)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Errorf("explicit --model must not carry a fallback note, got %q", note)
	}
	if target.Model != "explicit-model" {
		t.Errorf("target.Model = %q, want the explicit override, not the COPILOT_MODEL pin", target.Model)
	}
}

// A github-copilot login stored by opencode is its own leg, ahead of both the session provider
// and the generic own-login fallback above: it resolves to a real github-copilot target (no
// ErrOwnLogin), since the harness, not opencode, now owns writing that route's config.
func TestOpencodeCopilotLoginTakesPrecedence(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PATH", t.TempDir()) // no real opencode: OpencodeAuth must read the auth.json fixture, not a 2.x CLI
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setPin(t, providers.PinProfile, "max")
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated login (anthropic) is present too: github-copilot still wins.
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"},"anthropic":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !OpencodeCopilotLoginPresent() {
		t.Fatal("OpencodeCopilotLoginPresent() = false, want true")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	target, note, err := ResolveTarget(reg, "opencode", sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if target.Provider == nil || target.Provider.Name != "personal" {
		t.Fatalf("target.Provider = %+v, want github-copilot", target.Provider)
	}
	if !strings.Contains(note, "opencode auth login") {
		t.Errorf("note = %q, want it to name opencode auth login", note)
	}
	// --provider is explicit: the github-copilot login leg must not override an explicit choice.
	explicitTarget, _, err := ResolveTarget(reg, "opencode", sel, true)
	if err != nil {
		t.Fatalf("ResolveTarget (explicit): %v", err)
	}
	if explicitTarget.Provider != nil && explicitTarget.Provider.Name == "personal" && sel.Provider.Name != "personal" {
		t.Errorf("an explicit provider was overridden by the github-copilot login leg: %+v", explicitTarget)
	}
}

// OpencodeCopilotModelIDs swaps the registry's "auto" haiku default for the provider's OpencodeFallback,
// while the sonnet slot's own (non-"auto") id passes through unchanged. The production registry's
// github-copilot entry no longer defaults haiku to "auto" (it now pins a concrete id), so this overlays
// a synthetic one to keep exercising the swap itself, independent of that literal.
func TestOpencodeCopilotModelIDsSwapsAutoForFallback(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, k := range []string{"HARNESS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL"} {
		t.Setenv(k, "")
	}
	overlay := `{"providers":{"github":{"personal":{"kind":"openai","base_url":"https://api.githubcopilot.com",` +
		`"auth":{"type":"cli","login":"copilot"},` +
		`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"},` +
		`"opencode_fallback":"mai-code-1.1-flash"}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	main, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
	}
	if main != "claude-sonnet-5.5" {
		t.Errorf("main = %q, want the registry's own sonnet id unchanged", main)
	}
	if small != "mai-code-1.1-flash" {
		t.Errorf("small = %q, want the haiku slot's \"auto\" swapped for OpencodeFallback", small)
	}
}

// A pin that resolves the haiku slot to a real (non-"auto") id passes through untouched, even though
// the registry's own haiku default is "auto".
func TestOpencodeCopilotModelIDsLeavesNonAutoPinUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setPin(t, providers.SlotPin("haiku"), "claude-haiku-4.5")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, small, ok := OpencodeCopilotModelIDs(reg)
	if !ok {
		t.Fatal("OpencodeCopilotModelIDs: ok = false")
	}
	if small != "claude-haiku-4.5" {
		t.Errorf("small = %q, want the pinned id unchanged (not swapped for OpencodeFallback)", small)
	}
}

// A model set in the user's own opencode.jsonc is no longer a fallback leg: DescribeRoute's chain
// (copilot-login -> anthropic-key -> none) replaced it, so it has no effect on routing.
func TestOpencodeOwnConfigModelIsNoLongerAFallbackLeg(t *testing.T) {
	_, cfg := isolateOpencodeRouteEnv(t)
	setPin(t, providers.PinProfile, "max")
	if err := os.MkdirAll(filepath.Join(cfg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "opencode", "opencode.jsonc"), []byte(`{"model":"google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel := subscriptionSelection(t, reg)
	target, _, err := ResolveTarget(reg, "opencode", sel, false)
	if errors.Is(err, ErrOwnLogin) {
		t.Fatal("opencode.jsonc's own model must not resolve to ErrOwnLogin under the new chain")
	}
	if err == nil {
		t.Fatalf("want the LegNone error with no credential present; got target=%+v", target)
	}
	if !strings.Contains(err.Error(), "claude-code configure github") {
		t.Errorf("error = %q, want it to name `claude-code configure github`", err)
	}
}

// Regression: ConfigureHeadless on opencode's own GitHub Copilot login must not surface ErrOwnLogin (the
// headless runner has no interactive menu to pick a model from) and must carry the resolved model as
// `-m github-copilot/<model>`, not silently fall back to opencode's own default.
func TestOpencodeCopilotLoginHeadlessCarriesModel(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PATH", t.TempDir()) // no real opencode: OpencodeAuth must read the auth.json fixture, not a 2.x CLI
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setPin(t, providers.PinProfile, "max")
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{"github-copilot":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := ResolveTarget(reg, "opencode", sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	_, argv, err := ConfigureHeadless(context.Background(), "opencode", target, Headless{Prompt: "pong"})
	if errors.Is(err, ErrOwnLogin) {
		t.Fatal("ConfigureHeadless must not surface ErrOwnLogin: the headless runner cannot fall back to an interactive model menu")
	}
	if err != nil {
		t.Fatalf("ConfigureHeadless: %v", err)
	}
	wantModel := target.Model
	if wantModel == "auto" && target.Provider != nil && target.Provider.OpencodeFallback != "" {
		wantModel = target.Provider.OpencodeFallback
	}
	want := "github-copilot/" + wantModel
	if !slices.Contains(argv, want) {
		t.Errorf("argv = %v, want it to carry -m %q", argv, want)
	}
	// Interactive (h == nil, via Configure) keeps the old behavior: opencode launches plain on its own login.
	if _, _, err := Configure(context.Background(), "opencode", target, ""); !errors.Is(err, ErrOwnLogin) {
		t.Errorf("Configure (interactive) err = %v, want ErrOwnLogin", err)
	}
}

// setPin seeds (value != "") or clears a vault pin; pins are vault-only, never read from env.
func setPin(t *testing.T, key, value string) {
	t.Helper()
	if err := providers.SetPins(map[string]string{key: value}); err != nil {
		t.Fatal(err)
	}
}
