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

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const overlay = `{
  "default": "gw",
  "fallback": ["anthropic"],
  "providers": {"anthropic": {
    "gateway":{"protocol":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"key","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}},
    "gw": {"protocol": "anthropic", "base_url": "https://gw.example.com",
      "routes": {"openai": "/v1", "responses": "https://other.example.com/r/"},
      "auth": {"type":"key", "env": "GW_KEY"}, "headers": {"X-Team": "a"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5"}}},
    "nvidia": {"nvidia": null}
  }
}`

func load(t *testing.T) *Registry {
	t.Helper()
	vaulttest.UseKeyring(t)         // scratch config dir + in-memory keyring: never the real OS keyring
	t.Setenv(EnvSessionProfile, "") // a session launched with a provider pin must not leak into the registry tests
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

// the github profile's embedded default pins OpencodeVariant to "medium", the opencode model-variant suffix.
func TestEmbeddedDefaultsGithubCopilotOpencodeVariant(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Providers["personal"]
	if !ok {
		t.Fatal("personal (github) profile missing from embedded defaults")
	}
	if p.OpencodeVariant != "medium" {
		t.Errorf("OpencodeVariant = %q, want %q", p.OpencodeVariant, "medium")
	}
}

func TestOverlayAndRoutes(t *testing.T) {
	reg := load(t)
	if _, ok := reg.Providers["nvidia"]; ok {
		t.Error("null overlay entry should remove the built-in")
	}
	gw, _ := reg.Get("gw")
	for d, want := range map[string]string{
		ProtocolAnthropic: "https://gw.example.com",
		ProtocolOpenAI:    "https://gw.example.com/v1",
		ProtocolResponses: "https://other.example.com/r",
	} {
		if got, ok := gw.Route(d); !ok || got != want {
			t.Errorf("Route(%s)=%q,%v want %q", d, got, ok, want)
		}
	}
	if _, ok := gw.Route(ProtocolGemini); ok {
		t.Error("gemini route should be absent")
	}
	if err := (&Registry{Default: "nope", Providers: map[string]*Provider{}}).Validate(); err == nil {
		t.Error("dangling default must fail validation")
	}
	keyedLogin := &Provider{Protocol: ProtocolOpenAI, BaseURL: "https://x", Auth: Auth{Type: AuthKey, Login: "codex"}}
	if err := (&Registry{Providers: map[string]*Provider{"x": keyedLogin}}).Validate(); err == nil {
		t.Error("auth.login without passthrough must fail validation")
	}
}

// opencode_fallback exists to substitute for a catalog "auto" id opencode can't resolve itself; letting
// opencode_fallback itself be "auto" would recreate the exact value it's supposed to replace.
func TestValidateRejectsOpencodeFallbackAuto(t *testing.T) {
	bad := &Provider{Protocol: ProtocolAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "X_KEY"}, OpencodeFallback: "auto"}
	err := (&Registry{Providers: map[string]*Provider{"x": bad}}).Validate()
	if err == nil {
		t.Fatal("opencode_fallback: \"auto\" must fail validation")
	}
	if !strings.Contains(err.Error(), "opencode_fallback cannot itself be") {
		t.Errorf("error = %q, want it to name the opencode_fallback=auto rule", err)
	}
	good := &Provider{Protocol: ProtocolAnthropic, BaseURL: "https://x", Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "X_KEY"}, OpencodeFallback: "mai-code-1.1-flash"}
	if err := (&Registry{Providers: map[string]*Provider{"x": good}}).Validate(); err != nil {
		t.Errorf("a real opencode_fallback id must pass validation, got %v", err)
	}
}

// an overlay's opencodeLadder/opencodePlugins replace the defaults' whole, not merge into them.
func TestOverlayReplacesOpencodeLadderAndPlugins(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(EnvSessionProfile, "")
	base := []byte(`{"providers": {"anthropic": {"a": {"protocol": "anthropic", "base_url": "https://a", "auth": {"type": "none"}},
		"b": {"protocol": "anthropic", "base_url": "https://b", "auth": {"type": "none"}}}},
		"routing": {"opencode": {"ladder": ["opencode-go"]}}, "opencodePlugins": ["old-plugin"]}`)
	overlay := []byte(`{"routing": {"opencode": {"ladder": ["openai", "opencode-go"]}}, "opencodePlugins": ["new-plugin"]}`)
	reg, err := Parse(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reg.Routing.Opencode.LadderOrder(), []string{"openai", "opencode-go"}) {
		t.Errorf("OpencodeLadder = %v, want overlay's full replacement", reg.Routing.Opencode.LadderOrder())
	}
	if !reflect.DeepEqual(reg.OpencodePlugins, []string{"new-plugin"}) {
		t.Errorf("OpencodePlugins = %v, want overlay's full replacement", reg.OpencodePlugins)
	}
}

// TestOverlayLadderNormalizesLegacyProviderNamesAndDropsDuplicates: an overlay ladder may still name a
// registry provider ("chatgpt") instead of its opencode sub id ("openai") — Parse must map it through
// NormalizeOpencodeLadder before Validate ever sees it, and a duplicate that only appears after normalizing
// (two "chatgpt" entries, or a "chatgpt"/"openai" collision) must collapse to one, first occurrence wins.
func TestOverlayLadderNormalizesLegacyProviderNamesAndDropsDuplicates(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(EnvSessionProfile, "")
	base := []byte(`{"providers": {
		"openai": {"chatgpt": {"protocol": "openai", "base_url": "https://a", "auth": {"type": "none"}, "opencodeID": "openai"}},
		"github": {"personal": {"protocol": "openai", "base_url": "https://b", "auth": {"type": "none"}, "opencodeID": "github-copilot"}}
	}, "routing": {"opencode": {"ladder": ["opencode-go"]}}}`)
	overlay := []byte(`{"routing": {"opencode": {"ladder": ["chatgpt", "personal", "chatgpt"]}}}`)
	reg, err := Parse(base, overlay)
	if err != nil {
		t.Fatalf("Parse() = %v, want the legacy provider-name ladder to normalize and validate", err)
	}
	if !reflect.DeepEqual(reg.Routing.Opencode.LadderOrder(), []string{"openai", "github-copilot"}) {
		t.Errorf("OpencodeLadder = %v, want [\"openai\", \"github-copilot\"] (normalized, deduplicated)", reg.Routing.Opencode.LadderOrder())
	}

	// an id that names nothing real must still fail validation after normalization.
	badOverlay := []byte(`{"routing": {"opencode": {"ladder": ["ghost"]}}}`)
	if _, err := Parse(base, badOverlay); err == nil {
		t.Error("an unknown opencodeLadder id must still error after normalization")
	}
}

func TestValidateRejectsUnknownOpencodeLadderName(t *testing.T) {
	ghostLadder := []string{"ghost"}
	reg := &Registry{Routing: Routing{Opencode: OpencodeRouting{Ladder: &ghostLadder}}, Providers: map[string]*Provider{}}
	err := reg.Validate()
	if err == nil {
		t.Fatal("unknown opencodeLadder name must fail validation")
	}
	if !strings.Contains(err.Error(), "routing.opencode.ladder names unknown opencode sub id") {
		t.Errorf("error = %q, want it to name the opencodeLadder rule", err)
	}
}

// TestValidateAcceptsOpencodeSubIDsWithoutProviders: ladder entries are opencode sub ids, not registry provider names.
func TestValidateAcceptsOpencodeSubIDsWithoutProviders(t *testing.T) {
	defaultOrderLadder := []string{"opencode-go", "github-copilot", "openai"}
	reg := &Registry{Routing: Routing{Opencode: OpencodeRouting{Ladder: &defaultOrderLadder}}, Providers: map[string]*Provider{}}
	if err := reg.Validate(); err != nil {
		t.Fatalf("default-order sub ids rejected: %v", err)
	}
}

func TestSelectPinsAndCrossProvider(t *testing.T) {
	reg := load(t)
	vaulttest.SetRecord(t, "claude", "model", "claude-opus-4-8")
	vaulttest.SetRecord(t, "claude", "aliases.haiku", "openai:gpt-5-nano")
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
		t.Error("cross-provider alias must need the router")
	}
	if tgt := ParseTarget(reg, "qwen3:30b[1m]", sel.Provider); tgt.Provider != sel.Provider || tgt.Model != "qwen3:30b" {
		t.Errorf("unregistered prefix must stay a model: %+v", tgt)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if got := AgentRecord("claude").Model; got != "claude-opus-4-8" {
			t.Errorf("vault pin = %q; want claude-opus-4-8", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A passthrough (login) session never routes on the fallback ladder alone: the router's base URL disables Remote Control.
func TestNeedsRouterPassthroughFallback(t *testing.T) {
	reg := &Registry{Fallback: []string{"gw"}, Providers: map[string]*Provider{
		"sub": {Name: "sub", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: AuthCLI}, Models: map[string]string{"opus": "o"}},
		"key": {Name: "key", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "K"}, Models: map[string]string{"opus": "o"}},
	}}
	for name, want := range map[string]bool{"sub": false, "key": true} {
		p := reg.Providers[name]
		sel := Selection{Provider: p, Aliases: map[string]Target{"opus": {Provider: p, Model: "o"}}}
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
		t.Errorf("alias mapping: %+v", tg)
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
	if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "from-vault" || h.Get("X-Team") != "a" {
		t.Errorf("headers %v", h)
	}
	t.Setenv("GW_KEY", "from-env")
	if v, _ := Credential(context.Background(), gw); v != "from-env" {
		t.Errorf("env must beat the vault, got %q", v)
	}
	gw.Auth = Auth{Type: AuthKey, Command: "echo minted"}
	if v, _ := Credential(context.Background(), gw); v != "minted" {
		t.Errorf("command cred %q", v)
	}
	sub, _ := reg.Get("max")
	h = http.Header{"Authorization": {"Bearer client"}}
	if err := Authorize(context.Background(), sub, h); err != nil || h.Get("Authorization") != "Bearer client" {
		t.Errorf("passthrough must keep client auth: %v %v", err, h)
	}
}

// A Provider marshals its Headers redacted at the type; an inline gateway token never reaches a dump.
func TestProviderMarshalRedactsHeaders(t *testing.T) {
	tok := "sk" + "-abcdefghijklmnopqrstuvwxyz0123"
	p := Provider{Protocol: ProtocolOpenAI, BaseURL: "https://gw", Headers: map[string]string{"X-Key": tok, "X-Plain": "v"}}
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
	vaulttest.Put(t, vault.Pins, "A", "1")
	before, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := PinDiff(map[string]string{"A": "1", "B": "x"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "B: (unset) → x" || !bytes.Equal(before, after) {
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

	p := &Provider{Auth: Auth{Type: AuthKey, Env: "K"}}
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
		t.Error("Demote must not return the same model under another alias")
	}
}

// TestModelDeniedVsIsQuota pins the split: ModelDenied is the narrow "this model is dead on this key, try a
// lower alias" set; IsQuota is the broader "the account itself is exhausted" superset that also includes the
// window/plan/credit markers ModelDenied must NOT match (those don't mean a lower alias would fare better).
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
	// A body carrying BOTH a hard account-exhaustion marker and a model-denied marker (OpenAI's 402 body
	// reads this way: a 402 wrapping "insufficient_quota") is account-exhausted, not model-denied — demoting
	// to a lower alias on the same dead account would only fail again, so ModelDenied must veto.
	hardOverridesModel := []string{
		`HTTP 402 payment required: insufficient_quota`,
		`out of credits`,
	}
	for _, text := range hardOverridesModel {
		if !IsQuota(text) {
			t.Errorf("IsQuota(%q) = false, want true", text)
		}
		if ModelDenied(text) {
			t.Errorf("ModelDenied(%q) = true, want false (hard account-exhaustion marker vetoes)", text)
		}
	}
	// Gemini's free-tier per-model denial reuses accountRE's "exceeded your current quota" phrase alongside
	// "limit: 0" — hardAccountRE deliberately excludes "current" (only monthly/weekly), so this one stays
	// model-denied: a lower alias on the same key can still serve.
	if geminiFreeTier := `exceeded your current quota, limit: 0`; !ModelDenied(geminiFreeTier) {
		t.Errorf("ModelDenied(%q) = false, want true (model-specific denial, not account-wide)", geminiFreeTier)
	}
}

// TestValidateRejectsUnknownRouteTierAndAgent checks Registry.Validate catches an overlay cell naming a
// agent or alias outside RouteAgents/RouteAliases before it ever reaches router.LoadFrom.
func TestValidateRejectsUnknownRouteTierAndAgent(t *testing.T) {
	badTier := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{"opencode": {"ultra": {Model: "x"}}}}}
	if err := badTier.Validate(); err == nil || !strings.Contains(err.Error(), "unknown alias") {
		t.Errorf("unknown alias: err = %v, want it to name the rule", err)
	}
	badAgent := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{"ghost": {"sonnet": {Model: "x"}}}}}
	if err := badAgent.Validate(); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("unknown agent: err = %v, want it to name the rule", err)
	}
}

// TestValidateRejectsUnknownAccountKeyAndOpencodePrefix checks Registry.Validate catches an explicit
// account_key naming no known billing entry and an opencode cell whose override model prefix has no
// derivable billing account, while letting a "github·" dynamic key and an off-agent "/" model override
// (no account-key rule applies outside opencode) through.
func TestValidateRejectsUnknownAccountKeyAndOpencodePrefix(t *testing.T) {
	badAccount := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{
		"opencode": {"sonnet": {Model: "opencode-go/y", Account: "nobody:nothing"}},
	}}}
	if err := badAccount.Validate(); err == nil || !strings.Contains(err.Error(), `account_key "nobody:nothing"`) {
		t.Errorf("unknown account_key: err = %v, want it to name the rule", err)
	}
	badPrefix := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{
		"opencode": {"sonnet": {Model: "zed/model"}},
	}}}
	if err := badPrefix.Validate(); err == nil || !strings.Contains(err.Error(), `model "zed/model" prefix has no known billing account`) {
		t.Errorf("unknown opencode prefix: err = %v, want it to name the rule", err)
	}
	copilotUser := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{
		"opencode": {"sonnet": {Model: "github-copilot/x", Account: "github·anyuser"}},
	}}}
	if err := copilotUser.Validate(); err != nil {
		t.Errorf("github·anyuser must pass (dynamic family): %v", err)
	}
	offRunnerSlash := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Cells: map[string]map[string]RouteCell{
		"codex": {"sonnet": {Model: "openrouter/some-model"}},
	}}}
	if err := offRunnerSlash.Validate(); err != nil {
		t.Errorf("a non-opencode \"/\" model with no account_key must pass (rule is opencode-scoped): %v", err)
	}
}

// TestValidateRejectsOutOfRangeRouteWeight checks an overlay weight outside [0,1] (or one that doesn't sum
// to 1 across the dimensions) fails Validate, rather than silently reaching router with a bad score.
func TestValidateRejectsOutOfRangeRouteWeight(t *testing.T) {
	outOfRange := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Weights: map[string]float64{"price": 1.5, "headroom": -0.5}}}
	if err := outOfRange.Validate(); err == nil || !strings.Contains(err.Error(), "outside [0,1]") {
		t.Errorf("out-of-range weight: err = %v, want it to name the rule", err)
	}
	doesNotSum := &Registry{Providers: map[string]*Provider{}, Routing: Routing{Weights: map[string]float64{"price": 0.2, "headroom": 0.2}}}
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
		{AuthKey, true},
		{AuthCLI, false},
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

func TestProviderIsTheRowsOwnField(t *testing.T) {
	if got := (Provider{Provider: "github"}).provider(); got != "github" {
		t.Errorf("provider() = %q, want github", got)
	}
	if got := (Provider{Protocol: ProtocolOpenAI, BaseURL: "https://api.openai.com/v1"}).provider(); got != "" {
		t.Errorf("a Go-built row without a provider field = %q, want empty", got)
	}
}

// Claude's own limit messages are account quota; a fast-mode limit is not.
func TestIsQuotaClaudeMessages(t *testing.T) {
	for text, want := range map[string]bool{
		"You've hit your limit · resets 3pm":             true,
		"Out of usage credits":                           true,
		"You've hit your channel's monthly spend limit.": true,
		"You've hit your fast limit":                     false,
	} {
		if got := IsQuota(text); got != want {
			t.Errorf("IsQuota(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestAuthMappingTable pins every on-disk auth spelling to its Method and header, and back.
func TestAuthMappingTable(t *testing.T) {
	cases := []struct {
		name, kind, auth, method, header, wr string
	}{
		{"cli", "anthropic", `{"type":"cli"}`, AuthCLI, "", `"type":"cli"`},
		{"key anthropic default header", "anthropic", `{"type":"key"}`, AuthKey, HeaderXAPIKey, `"header":"x-api-key"`},
		{"key openai default header", "openai", `{"type":"key"}`, AuthKey, HeaderBearer, `"header":"bearer"`},
		{"key explicit header", "openai", `{"type":"key","header":"x-api-key"}`, AuthKey, HeaderXAPIKey, `"header":"x-api-key"`},
		{"none", "openai", `{"type":"none"}`, AuthNone, "", `"type":"none"`},
		{"oauth", "anthropic", `{"type":"oauth"}`, AuthOAuth, HeaderBearer, `"type":"oauth"`},
		{"m2m", "anthropic", `{"type":"m2m","clientId":"c"}`, AuthM2M, HeaderBearer, `"type":"m2m"`},
		{"pat", "anthropic", `{"type":"pat"}`, AuthPAT, HeaderBearer, `"type":"pat"`},
	}
	for _, c := range cases {
		var p Provider
		if err := json.Unmarshal([]byte(`{"protocol":"`+c.kind+`","auth":`+c.auth+`}`), &p); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Auth.Method() != c.method || p.Auth.HeaderStyle() != c.header {
			t.Errorf("%s: method=%q header=%q; want %q %q", c.name, p.Auth.Method(), p.Auth.HeaderStyle(), c.method, c.header)
		}
		out, _ := json.Marshal(p.Auth)
		if !strings.Contains(string(out), c.wr) {
			t.Errorf("%s: wrote %s, want it to contain %s", c.name, out, c.wr)
		}
	}
}

func TestAPIKeyAuthStaysTrueForTokenSendingRows(t *testing.T) {
	for _, auth := range []string{`{"type":"key"}`, `{"type":"pat"}`, `{"type":"m2m","clientId":"c"}`, `{"type":"oauth"}`} {
		var p Provider
		if err := json.Unmarshal([]byte(`{"protocol":"openai","auth":`+auth+`}`), &p); err != nil || !p.APIKeyAuth() {
			t.Errorf("%s: APIKeyAuth = %v, %v", auth, p.APIKeyAuth(), err)
		}
	}
	for _, auth := range []string{`{"type":"cli"}`, `{"type":"none"}`} {
		var p Provider
		_ = json.Unmarshal([]byte(`{"protocol":"openai","auth":`+auth+`}`), &p)
		if p.APIKeyAuth() {
			t.Errorf("%s: APIKeyAuth must be false", auth)
		}
	}
}

func TestSessionProfileIsAnEnvMarkerNotAPin(t *testing.T) {
	load(t)
	t.Setenv(EnvSessionProfile, "")
	vaulttest.SetRecord(t, "claude", "profile", "dev")
	if SessionProfile() != "" {
		t.Error("the vault pin leaked into SessionProfile")
	}
	t.Setenv(EnvSessionProfile, " qa ")
	if SessionProfile() != "qa" {
		t.Errorf("SessionProfile = %q", SessionProfile())
	}
	if got := AgentRecord("codex").Model; got != "" {
		t.Errorf("unset pin = %q", got)
	}
}

func TestSecretName(t *testing.T) {
	cases := []struct{ provider, profile, method, want string }{
		{"databricks", "prod", AuthOAuth, "DATABRICKS_PROD_OAUTH"},
		{"databricks", "dbx-dev.1", AuthM2M, "DATABRICKS_DBX_DEV_1_M2M"},
		{"databricks", "qa", AuthPAT, "DATABRICKS_QA_PAT"},
		{"anthropic", "anthropic", AuthKey, "ANTHROPIC_KEY"}, // collapse: profile == provider
		{"nvidia", "nvidia", AuthKey, "NVIDIA_KEY"},          // collapse
		{"anthropic", "work", AuthKey, "ANTHROPIC_WORK_KEY"}, // no collapse
		{"openai", "openai", AuthKey, "OPENAI_KEY"},          // collapse
		{"github", "personal", AuthOAuth, "GITHUB_PERSONAL_OAUTH"},
		{"anthropic", "max", AuthCLI, ""},
		{"", "x", AuthKey, ""},
	}
	for _, c := range cases {
		if got := SecretName(c.provider, c.profile, c.method); got != c.want {
			t.Errorf("SecretName(%s,%s,%s) = %q, want %q", c.provider, c.profile, c.method, got, c.want)
		}
	}
}

func TestOAuthValueRoundTrip(t *testing.T) {
	p := mintRow(t, "rt-prod", AuthOAuth, "", "http://127.0.0.1:1")
	if err := writeOAuth(p, oauthValue{Refresh: "new-refresh", Access: "new-access"}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := vault.ReadSecret("DATABRICKS_RT_PROD_OAUTH")
	var stored map[string]string
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored["refresh"] != "new-refresh" || stored["access"] != "new-access" {
		t.Fatalf("stored value = %q (%v), want one {refresh, access} JSON", raw, err)
	}
	if ov, _ := readOAuth(p); ov.Refresh != "new-refresh" || ov.Access != "new-access" {
		t.Errorf("after write = %+v", ov)
	}
}

func TestKeyRowCredentialFallsBackToAuthEnv(t *testing.T) {
	load(t)
	p := &Provider{Name: "anthropic", Provider: "anthropic", Protocol: ProtocolAnthropic, Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "ANTHROPIC_API_KEY"}}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if CredentialPresent(p) {
		t.Fatal("present before any secret")
	}
	if err := secretstore.Store("ANTHROPIC_API_KEY", "old-key"); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(p) {
		t.Error("old env-named secret must count")
	}
	if err := secretstore.Store("ANTHROPIC_KEY", "new-key"); err != nil {
		t.Fatal(err)
	}
	if v, err := Credential(t.Context(), p); err != nil || v != "new-key" {
		t.Errorf("Credential = %q, %v; want the new-named secret first", v, err)
	}
}

func TestReadOAuthCorruptSecretIsAnError(t *testing.T) {
	p := mintRow(t, "corrupt-oauth", AuthOAuth, "", "http://127.0.0.1:1")
	if err := secretstore.Store(AuthSecretName(p), "not-json{"); err != nil {
		t.Fatal(err)
	}
	if _, err := readOAuth(p); err == nil {
		t.Fatal("readOAuth returned nil error for an unparseable existing secret")
	}
}

func TestCopilotModelNilRegistry(t *testing.T) {
	vaulttest.UseKeyring(t)
	if m, _ := CopilotModel(nil); m != "" {
		t.Fatalf("model = %q, want empty", m)
	}
}

func TestSealAccessKeepsUnreadableOAuthValue(t *testing.T) {
	p := mintRow(t, "seal-unreadable", AuthOAuth, "", "http://127.0.0.1:1")
	if err := secretstore.Store(AuthSecretName(p), "not-json{"); err != nil {
		t.Fatal(err)
	}
	sealAccess(p, "new-access")
	raw, _, err := vault.ReadSecret(AuthSecretName(p))
	if err != nil || raw != "not-json{" {
		t.Fatalf("sealed value = %q, %v; want it untouched after an unreadable read", raw, err)
	}
}

const wireHost = "https://example.cloud.databricks.com"

func wireProvider(t *testing.T, opts string) *Provider {
	t.Helper()
	reg := mustParse(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "`+wireHost+`"`+opts+`}}}}`)
	return reg.Providers["dbx"]
}

func TestWireGatewayDerivation(t *testing.T) {
	p := wireProvider(t, `, "options": {"wire": "ai-gateway"}`)
	if p.BaseURL != wireHost+"/ai-gateway/anthropic" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if got := p.Routes[ProtocolResponses]; got != wireHost+"/ai-gateway/codex/v1" {
		t.Errorf("responses route = %q", got)
	}
	if _, ok := p.Routes[ProtocolOpenAI]; ok {
		t.Errorf("gateway must have no openai route: %v", p.Routes)
	}
	if p.ModelPrefix != "system.ai." {
		t.Errorf("ModelPrefix = %q", p.ModelPrefix)
	}
	if p.Headers["x-databricks-use-coding-agent-mode"] != "true" {
		t.Errorf("headers = %v", p.Headers)
	}
	if got := p.Models["sonnet"]; got != "system.ai.claude-sonnet-5-5" {
		t.Errorf("sonnet alias = %q", got)
	}
}

func TestWireServingDefaultUnchanged(t *testing.T) {
	for _, opts := range []string{``, `, "options": {"wire": "serving"}`} {
		p := wireProvider(t, opts)
		if p.BaseURL != wireHost+"/serving-endpoints/anthropic" {
			t.Errorf("%q: BaseURL = %q", opts, p.BaseURL)
		}
		for _, d := range []string{ProtocolOpenAI, ProtocolResponses} {
			if p.Routes[d] != wireHost+"/serving-endpoints" {
				t.Errorf("%q: route %s = %q", opts, d, p.Routes[d])
			}
		}
		if p.ModelPrefix != "databricks-" {
			t.Errorf("%q: prefix = %q", opts, p.ModelPrefix)
		}
		if _, ok := p.Headers["x-databricks-use-coding-agent-mode"]; ok {
			t.Errorf("%q: serving must not set the gateway header", opts)
		}
	}
}

func TestWireUnknownRejected(t *testing.T) {
	_, err := parseOverlay(t, `{"providers": {"databricks": {"dbx": {"preset": "databricks", "host": "`+wireHost+`", "options": {"wire": "bogus"}}}}}`)
	if err == nil || !strings.Contains(err.Error(), "options.wire") {
		t.Fatalf("err = %v, want options.wire rejection", err)
	}
}

func TestWireModelPrefixThenDots(t *testing.T) {
	serving := wireProvider(t, ``)
	gateway := wireProvider(t, `, "options": {"wire": "ai-gateway"}`)
	cases := []struct {
		p        *Provider
		in, want string
	}{
		{serving, "gpt-5.5", "databricks-gpt-5-5"},
		{serving, "claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{gateway, "gpt-5.5", "system.ai.gpt-5-5"},
		{gateway, "claude-sonnet-5-5", "system.ai.claude-sonnet-5-5"},
		{gateway, "system.ai.gpt-5-5", "system.ai.gpt-5-5"}, // idempotent
	}
	for _, c := range cases {
		if got := c.p.WireModel(c.in); got != c.want {
			t.Errorf("%s WireModel(%q) = %q, want %q", c.p.ModelPrefix, c.in, got, c.want)
		}
	}
}

func TestWireNonPresetUntouched(t *testing.T) {
	p := Provider{ModelPrefix: "x."}
	if got := p.WireModel("gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("non-preset WireModel = %q", got)
	}
}

const dbxProv = `"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work"}`

func dbxProvider(t *testing.T) Provider {
	t.Helper()
	return *mustParse(t, `{"providers":{"databricks":{`+dbxProv+`}}}`).Providers["dbx"]
}

func TestWireModelTable(t *testing.T) {
	db := dbxProvider(t)
	plain := Provider{}
	cases := []struct {
		name string
		p    Provider
		in   string
		want string
	}{
		{"canonical", db, "claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"already wire", db, "databricks-claude-opus-5-5", "databricks-claude-opus-5-5"},
		{"1m kept", db, "claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"wire 1m unchanged", db, "databricks-claude-opus-5-5[1m]", "databricks-claude-opus-5-5[1m]"},
		{"slash qualified", db, "openai/gpt-5", "openai/gpt-5"},
		{"colon qualified", db, "openai:gpt-5", "openai:gpt-5"},
		{"empty", db, "", ""},
		{"dots to dashes", db, "gpt-5.5", "databricks-gpt-5-5"},
		{"dots with 1m", db, "claude-opus-4.5[1m]", "databricks-claude-opus-4-5[1m]"},
		{"custom name unchanged", db, "my-opus", "my-opus"},
		{"custom dotted name unchanged", db, "my.endpoint", "my.endpoint"},
		{"catalog gemini", db, "gemini-3.8-flash", "databricks-gemini-3-8-flash"},
		{"non-catalog passes through", db, "gemini-2.5-pro", "gemini-2.5-pro"},
		{"no prefix provider", plain, "claude-opus-5-5", "claude-opus-5-5"},
		{"no prefix keeps dots", plain, "gpt-5.5", "gpt-5.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.p.WireModel(c.in)
			if got != c.want {
				t.Fatalf("WireModel(%q) = %q, want %q", c.in, got, c.want)
			}
			if again := c.p.WireModel(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestLoadDecoratesPresetModels(t *testing.T) {
	reg := mustParse(t, `{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net/","cliProfile":"work",
	  "models":{"opus":"claude-opus-5-5[1m]","sonnet":"gpt-5.5"}}}}}`)
	m := reg.Providers["dbx"].Models
	want := map[string]string{
		"opus": "databricks-claude-opus-5-5[1m]", "sonnet": "databricks-gpt-5-5", "haiku": "databricks-claude-haiku-5-5",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Models[%s] = %q, want %q", k, m[k], v)
		}
	}
	plain := mustParse(t, `{"providers":{"openai":{"p":{"protocol":"anthropic","base_url":"https://p","auth":{"type":"none"},"models":{"opus":"gpt-5.5"}}}}}`)
	if got := plain.Providers["p"].Models["opus"]; got != "gpt-5.5" {
		t.Errorf("plain provider model = %q", got)
	}
}

// The decorated ids live only in memory: the overlay writer works on the stored doc, never the registry.
func TestDecoratedModelsNeverPersisted(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	err := SetProviderFields("dbx", map[string]any{"preset": "databricks", "host": "https://adb-1.azuredatabricks.net/",
		"cliProfile": "work", "models": map[string]string{"opus": "claude-opus-5-5"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Providers["dbx"].Models["opus"]; got != "databricks-claude-opus-5-5" {
		t.Fatalf("loaded opus = %q, want decorated", got)
	}
	if err := SetProviderModel("dbx", "sonnet", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	doc, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "databricks-claude") {
		t.Errorf("overlay persisted decorated ids: %s", doc)
	}
	if !strings.Contains(string(doc), `"claude-opus-5-5"`) || !strings.Contains(string(doc), `"claude-sonnet-5-5"`) {
		t.Errorf("overlay lost the canonical ids: %s", doc)
	}
}

// The endpoint type alone decides who serves 1M: serving and first-party yes, ai-gateway and others no.
func TestServes1MByEndpointType(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Provider
		want bool
	}{
		{"nil", nil, false},
		{"databricks serving", &Provider{ModelPrefix: models.DatabricksPrefix}, true},
		{"first-party anthropic", &Provider{Provider: "anthropic"}, true},
		{"ai-gateway", &Provider{ModelPrefix: gatewayModelPrefix}, false},
		{"github copilot", &Provider{Provider: "github"}, false},
		{"non-Claude endpoint", &Provider{Provider: "ollama"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Serves1M(); got != tc.want {
				t.Errorf("Serves1M = %v, want %v", got, tc.want)
			}
		})
	}
}

// ContextWindow: a typed [1m] is always 1M; a bare catalog id is 1M only on a 1M endpoint; a non-Claude id is 200K.
func TestContextWindowByEndpoint(t *testing.T) {
	serving := &Provider{ModelPrefix: models.DatabricksPrefix}
	gateway := &Provider{ModelPrefix: gatewayModelPrefix}
	copilot := &Provider{Provider: "github"}
	for _, tc := range []struct {
		name string
		p    *Provider
		id   string
		want int
	}{
		{"serving 5.5", serving, "claude-opus-5-5", models.ContextWindow1M},
		{"gateway 5.5 bare", gateway, "claude-opus-5-5", models.ContextWindowStd},
		{"gateway typed [1m] kept", gateway, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"copilot bare", copilot, "claude-opus-5-5", models.ContextWindowStd},
		{"copilot typed [1m]", copilot, "claude-opus-5-5[1m]", models.ContextWindow1M},
		{"serving non-Claude", serving, "gpt-5", models.ContextWindowStd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContextWindow(tc.p, tc.id); got != tc.want {
				t.Errorf("ContextWindow(%q) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}
