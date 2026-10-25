package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const overlay = `{
  "default": "gw",
  "fallback": ["anthropic"],
  "providers": {
    "gateway":{"kind":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}},
    "gw": {"kind": "anthropic", "base_url": "https://gw.example.com",
      "routes": {"openai": "/v1", "responses": "https://other.example.com/r/"},
      "auth": {"type": "bearer", "env": "GW_KEY"}, "headers": {"X-Team": "a"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5"}},
    "ollama": null
  }
}`

func load(t *testing.T) *Registry {
	t.Helper()
	vaulttest.UseKeyring(t)   // scratch config dir + in-memory keyring: never the real OS keyring
	t.Setenv(PinProvider, "") // a session launched with a provider pin must not leak into the registry tests
	reg, err := Parse(defaultsJSON, []byte(overlay))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestEmbeddedDefaultsValid(t *testing.T) {
	if _, err := Parse(defaultsJSON, nil); err != nil {
		t.Fatal(err)
	}
}

// github-copilot's embedded default pins OpencodeVariant to "medium", the opencode model-variant suffix.
func TestEmbeddedDefaultsGithubCopilotOpencodeVariant(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Providers["github-copilot"]
	if !ok {
		t.Fatal("github-copilot provider missing from embedded defaults")
	}
	if p.OpencodeVariant != "medium" {
		t.Errorf("OpencodeVariant = %q, want %q", p.OpencodeVariant, "medium")
	}
}

func TestOverlayAndRoutes(t *testing.T) {
	reg := load(t)
	if _, ok := reg.Providers["ollama"]; ok {
		t.Error("null overlay entry should remove the built-in")
	}
	gw, _ := reg.Get("gw")
	for d, want := range map[string]string{
		DialectAnthropic: "https://gw.example.com",
		DialectOpenAI:    "https://gw.example.com/v1",
		DialectResponses: "https://other.example.com/r",
	} {
		if got, ok := gw.Route(d); !ok || got != want {
			t.Errorf("Route(%s)=%q,%v want %q", d, got, ok, want)
		}
	}
	if _, ok := gw.Route(DialectGemini); ok {
		t.Error("gemini route should be absent")
	}
	if err := (&Registry{Default: "nope", Providers: map[string]*Provider{}}).Validate(); err == nil {
		t.Error("dangling default must fail validation")
	}
	keyedLogin := &Provider{Kind: DialectOpenAI, BaseURL: "https://x", Auth: Auth{Type: AuthBearer, Login: "codex"}}
	if err := (&Registry{Providers: map[string]*Provider{"x": keyedLogin}}).Validate(); err == nil {
		t.Error("auth.login without passthrough must fail validation")
	}
}

// opencode_fallback exists to substitute for a catalog "auto" id opencode can't resolve itself; letting
// opencode_fallback itself be "auto" would recreate the exact value it's supposed to replace.
func TestValidateRejectsOpencodeFallbackAuto(t *testing.T) {
	bad := &Provider{Kind: DialectAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthAPIKey, Env: "X_KEY"}, OpencodeFallback: "auto"}
	err := (&Registry{Providers: map[string]*Provider{"x": bad}}).Validate()
	if err == nil {
		t.Fatal("opencode_fallback: \"auto\" must fail validation")
	}
	if !strings.Contains(err.Error(), "opencode_fallback cannot itself be") {
		t.Errorf("error = %q, want it to name the opencode_fallback=auto rule", err)
	}
	good := &Provider{Kind: DialectAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthAPIKey, Env: "X_KEY"}, OpencodeFallback: "mai-code-1.1-flash"}
	if err := (&Registry{Providers: map[string]*Provider{"x": good}}).Validate(); err != nil {
		t.Errorf("a real opencode_fallback id must pass validation, got %v", err)
	}
}

// an overlay's opencodeLadder/opencodePlugins replace the defaults' whole, not merge into them.
func TestOverlayReplacesOpencodeLadderAndPlugins(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(PinProvider, "")
	base := []byte(`{"providers": {"a": {"kind": "anthropic", "base_url": "https://a", "auth": {"type": "none"}},
		"b": {"kind": "anthropic", "base_url": "https://b", "auth": {"type": "none"}}},
		"opencodeLadder": ["a"], "opencodePlugins": ["old-plugin"]}`)
	overlay := []byte(`{"opencodeLadder": ["b", "a"], "opencodePlugins": ["new-plugin"]}`)
	reg, err := Parse(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reg.OpencodeLadder, []string{"b", "a"}) {
		t.Errorf("OpencodeLadder = %v, want overlay's full replacement", reg.OpencodeLadder)
	}
	if !reflect.DeepEqual(reg.OpencodePlugins, []string{"new-plugin"}) {
		t.Errorf("OpencodePlugins = %v, want overlay's full replacement", reg.OpencodePlugins)
	}
}

func TestValidateRejectsUnknownOpencodeLadderName(t *testing.T) {
	reg := &Registry{OpencodeLadder: []string{"ghost"}, Providers: map[string]*Provider{}}
	err := reg.Validate()
	if err == nil {
		t.Fatal("unknown opencodeLadder name must fail validation")
	}
	if !strings.Contains(err.Error(), "opencodeLadder names unknown provider") {
		t.Errorf("error = %q, want it to name the opencodeLadder rule", err)
	}
}

func TestSelectPinsAndCrossProvider(t *testing.T) {
	reg := load(t)
	t.Setenv(SlotPin("haiku"), "openai:gpt-5-nano")
	if err := SetPins(map[string]string{PinModel: "claude-opus-4-8"}); err != nil {
		t.Fatal(err)
	}
	sel, err := Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Provider.Name != "gw" {
		t.Fatalf("provider %s", sel.Provider.Name)
	}
	if got := sel.ClientModel("opus"); got != "claude-opus-4-8" { // gw has one_m unset
		t.Errorf("opus client model %q", got)
	}
	if got := sel.ClientModel("haiku"); got != "openai:gpt-5-nano" {
		t.Errorf("haiku client model %q", got)
	}
	if !sel.NeedsRouter(reg) {
		t.Error("cross-provider slot must need the router")
	}
	if tgt := ParseTarget(reg, "qwen3:30b[1m]", sel.Provider); tgt.Provider != sel.Provider || tgt.Model != "qwen3:30b" {
		t.Errorf("unregistered prefix must stay a model: %+v", tgt)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got, ok := tx.Get(vault.Pins, PinModel); !ok || got != "claude-opus-4-8" {
			t.Errorf("vault pin = %q, present %v; want claude-opus-4-8", got, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A passthrough (login) session never routes on the fallback ladder alone: the router's base URL disables Remote Control.
func TestNeedsRouterPassthroughFallback(t *testing.T) {
	reg := &Registry{Fallback: []string{"gw"}, Providers: map[string]*Provider{
		"sub": {Name: "sub", Kind: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: AuthPassthrough}, Models: map[string]string{"opus": "o"}},
		"key": {Name: "key", Kind: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: "x-api-key", Env: "K"}, Models: map[string]string{"opus": "o"}},
	}}
	for name, want := range map[string]bool{"sub": false, "key": true} {
		p := reg.Providers[name]
		sel := Selection{Provider: p, Slots: map[string]Target{"opus": {Provider: p, Model: "o"}}}
		if got := sel.NeedsRouter(reg); got != want {
			t.Errorf("%s: NeedsRouter = %v, want %v", name, got, want)
		}
	}
}

func TestFailoverTarget(t *testing.T) {
	reg := load(t)
	gw, _ := reg.Get("gw")
	or, _ := reg.Get("gateway")
	oa, _ := reg.Get("openai")
	if tg, ok := FailoverTarget(gw, "claude-sonnet-5", or); !ok || tg.Model != "anthropic/claude-sonnet-5" {
		t.Errorf("slot mapping: %+v", tg)
	}
	if tg, ok := FailoverTarget(gw, "claude-haiku-9-9", oa); !ok || tg.Model != "gpt-5-nano" {
		t.Errorf("family mapping: %+v", tg)
	}
}

func TestCredentialLadderAndAuthorize(t *testing.T) {
	reg := load(t)
	gw, _ := reg.Get("gw")
	if CredentialPresent(gw) {
		t.Fatal("no credential expected yet")
	}
	t.Setenv("GW_KEY", "")
	vaulttest.Secret(t, "GW_KEY", "from-vault")
	if !CredentialPresent(gw) {
		t.Fatal("a vault record must count as a credential")
	}
	h := http.Header{"Authorization": {"Bearer client"}}
	if err := Authorize(context.Background(), gw, h); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer from-vault" || h.Get("X-Team") != "a" {
		t.Errorf("headers %v", h)
	}
	t.Setenv("GW_KEY", "from-env")
	if v, _ := Credential(context.Background(), gw); v != "from-env" {
		t.Errorf("env must beat the vault, got %q", v)
	}
	gw.Auth = Auth{Type: AuthBearer, Command: "echo minted"}
	if v, _ := Credential(context.Background(), gw); v != "minted" {
		t.Errorf("command cred %q", v)
	}
	sub, _ := reg.Get("subscription")
	h = http.Header{"Authorization": {"Bearer oauth"}}
	if err := Authorize(context.Background(), sub, h); err != nil || h.Get("Authorization") != "Bearer oauth" {
		t.Errorf("passthrough must keep client auth: %v %v", err, h)
	}
}

// A Provider marshals its Headers redacted at the type; an inline gateway token never reaches a dump.
func TestProviderMarshalRedactsHeaders(t *testing.T) {
	tok := "sk" + "-abcdefghijklmnopqrstuvwxyz0123"
	p := Provider{Kind: DialectOpenAI, BaseURL: "https://gw", Headers: map[string]string{"X-Key": tok, "X-Plain": "v"}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, tok) || !strings.Contains(s, `"has_secret_headers":true`) || !strings.Contains(s, `"X-Plain":"v"`) {
		t.Errorf("marshal: %s", s)
	}
}

// PinDiff previews only real changes and never writes the vault.
func TestPinDiffPreviewsWithoutWriting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetPins(map[string]string{PinProvider: "gemini"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := PinDiff(map[string]string{PinProvider: "gemini", SlotPin("opus"): "x"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != SlotPin("opus")+": (unset) → x" || !bytes.Equal(before, after) {
		t.Fatalf("PinDiff = %q (vault changed: %v)", got, !bytes.Equal(before, after))
	}
}

// StoredSecret reads only the vault; Credential must prefer the process env over it.
func TestStoredSecretAndCredentialOrder(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("K", "")
	if got := StoredSecret("K"); got != "" {
		t.Errorf("StoredSecret (miss) = %q, want \"\"", got)
	}

	vaulttest.Secret(t, "K", "from-vault")
	if got := StoredSecret("K"); got != "from-vault" {
		t.Errorf("StoredSecret (vault hit) = %q, want from-vault", got)
	}

	p := &Provider{Auth: Auth{Type: AuthBearer, Env: "K"}}
	if v, err := Credential(context.Background(), p); err != nil || v != "from-vault" {
		t.Errorf("Credential (vault) = %q, %v; want from-vault", v, err)
	}
	t.Setenv("K", "from-env")
	if got := StoredSecret("K"); got != "from-vault" {
		t.Errorf("StoredSecret must ignore the process env, got %q", got)
	}
	if v, _ := Credential(context.Background(), p); v != "from-env" {
		t.Errorf("Credential must prefer env over the vault, got %q", v)
	}
}

func TestIsQuotaAndDemote(t *testing.T) {
	for text, want := range map[string]bool{
		`{"error":{"code":429,"message":"You exceeded your current quota ... limit: 0, model: gemini-4-argon"}}`: true,
		`insufficient_quota`:                 true,
		`429 Too Many Requests, retry in 2s`: false,
		`rate limit exceeded`:                false,
	} {
		if got := IsQuota(text); got != want {
			t.Errorf("IsQuota(%q) = %v, want %v", text, got, want)
		}
	}
	p := &Provider{Name: "g", Models: map[string]string{"opus": "big", "sonnet": "mid", "haiku": "small", "fable": "other"}}
	if d, ok := Demote(p, "big"); !ok || d.Model != "mid" {
		t.Errorf("Demote(big) = %v %v, want mid", d.Model, ok)
	}
	if d, ok := Demote(p, "mid"); !ok || d.Model != "small" {
		t.Errorf("Demote(mid) = %v %v, want small", d.Model, ok)
	}
	if _, ok := Demote(p, "small"); ok {
		t.Error("Demote(small) must stop at the bottom of the ladder")
	}
	if _, ok := Demote(&Provider{Name: "one", Models: map[string]string{"opus": "x", "sonnet": "x"}}, "x"); ok {
		t.Error("Demote must not return the same model under another slot")
	}
}

// TestModelDeniedVsIsQuota pins the split: ModelDenied is the narrow "this model is dead on this key, try a
// lower slot" set; IsQuota is the broader "the account itself is exhausted" superset that also includes the
// window/plan/credit markers ModelDenied must NOT match (those don't mean a lower slot would fare better).
func TestModelDeniedVsIsQuota(t *testing.T) {
	accountOnly := []string{
		`usage limit reached, try again in 60 seconds`,
		`HTTP 402 payment required`,
		`you have exceeded your monthly quota`,
	}
	for _, text := range accountOnly {
		if !IsQuota(text) {
			t.Errorf("IsQuota(%q) = false, want true (account-exhausted marker)", text)
		}
		if ModelDenied(text) {
			t.Errorf("ModelDenied(%q) = true, want false (account-exhausted, not model-denied)", text)
		}
	}
	modelDenied := []string{
		`limit: 0`,
		`insufficient_quota`,
		`quota exceeded for metric: generate_content_free_tier_requests`,
	}
	for _, text := range modelDenied {
		if !ModelDenied(text) {
			t.Errorf("ModelDenied(%q) = false, want true (model-denied marker)", text)
		}
		if !IsQuota(text) {
			t.Errorf("IsQuota(%q) = false, want true (model-denied implies account-exhausted via the OR)", text)
		}
	}
}

// TestValidateRejectsUnknownRouteTierAndRunner checks Registry.Validate catches an overlay cell naming a
// runner or tier outside RouteRunners/RouteTiers before it ever reaches routetable.LoadFrom.
func TestValidateRejectsUnknownRouteTierAndRunner(t *testing.T) {
	badTier := &Registry{Providers: map[string]*Provider{}, Route: map[string]map[string]RouteCell{"opencode": {"ultra": {Model: "x"}}}}
	if err := badTier.Validate(); err == nil || !strings.Contains(err.Error(), "unknown tier") {
		t.Errorf("unknown tier: err = %v, want it to name the rule", err)
	}
	badRunner := &Registry{Providers: map[string]*Provider{}, Route: map[string]map[string]RouteCell{"ghost": {"sonnet": {Model: "x"}}}}
	if err := badRunner.Validate(); err == nil || !strings.Contains(err.Error(), "unknown runner") {
		t.Errorf("unknown runner: err = %v, want it to name the rule", err)
	}
}

// TestValidateRejectsOutOfRangeRouteWeight checks an overlay weight outside [0,1] (or one that doesn't sum
// to 1 across the dimensions) fails Validate, rather than silently reaching routetable with a bad score.
func TestValidateRejectsOutOfRangeRouteWeight(t *testing.T) {
	outOfRange := &Registry{Providers: map[string]*Provider{}, RouteWeights: map[string]float64{"price": 1.5, "headroom": -0.5}}
	if err := outOfRange.Validate(); err == nil || !strings.Contains(err.Error(), "outside [0,1]") {
		t.Errorf("out-of-range weight: err = %v, want it to name the rule", err)
	}
	doesNotSum := &Registry{Providers: map[string]*Provider{}, RouteWeights: map[string]float64{"price": 0.2, "headroom": 0.2}}
	if err := doesNotSum.Validate(); err == nil || !strings.Contains(err.Error(), "sum to") {
		t.Errorf("non-summing weights: err = %v, want it to name the rule", err)
	}
}

// TestIsQuotaCoversEveryOldMarker checks the merged classifier still matches every marker either the old
// providers.HardQuota (the 4 "this key cannot use this model at all" phrasings) or the old route.IsQuota
// (the broader account-exhausted superset: 402, usage limit, monthly quota, ...) matched, and still
// rejects a plain transient rate limit.
func TestIsQuotaCoversEveryOldMarker(t *testing.T) {
	oldHardQuotaMarkers := []string{
		"limit: 0", "exceeded your current quota", "insufficient_quota", "quota exceeded for metric",
	}
	oldRouteIsQuotaMarkers := []string{
		"HTTP 402", "payment required", "exceeded your monthly quota", "exceeded your current limit",
		"exceeded your weekly quota", "usage limit", "insufficient_quota", "limit: 0",
		"quota exceeded for metric", "out of credits", "out of premium requests", "monthly quota",
	}
	for _, text := range append(oldHardQuotaMarkers, oldRouteIsQuotaMarkers...) {
		if !IsQuota(text) {
			t.Errorf("IsQuota(%q) = false, want true (an old marker)", text)
		}
	}
	for _, text := range []string{"429 Too Many Requests, retry in 2s", "rate limit exceeded", "line 4021"} {
		if IsQuota(text) {
			t.Errorf("IsQuota(%q) = true, want false (a plain rate limit, not a quota exhaustion)", text)
		}
	}
}

// TestAPIKeyAuth: only a harness-held credential (x-api-key or bearer) is a per-token request worth
// recording in the usage ledger — a vendor-CLI passthrough login (subscription) or no auth is not.
func TestAPIKeyAuth(t *testing.T) {
	cases := []struct {
		authType string
		want     bool
	}{
		{AuthAPIKey, true},
		{AuthBearer, true},
		{AuthPassthrough, false},
		{AuthNone, false},
		{"", false},
	}
	for _, c := range cases {
		p := Provider{Auth: Auth{Type: c.authType}}
		if got := p.APIKeyAuth(); got != c.want {
			t.Errorf("APIKeyAuth(type=%q) = %v, want %v", c.authType, got, c.want)
		}
	}
}
