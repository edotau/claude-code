package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hosttest"
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
    "google": {"antigravity": null}
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

func TestHeaderLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "nil"},
		{name: "empty", headers: map[string]string{}},
		{
			name:    "sorted newline separated",
			headers: map[string]string{"Z-Team": "last", "A-Empty": "", "M-Value": "with: colon"},
			want:    "A-Empty: \nM-Value: with: colon\nZ-Team: last",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeaderLines(tc.headers); got != tc.want {
				t.Fatalf("HeaderLines(%v) = %q, want %q", tc.headers, got, tc.want)
			}
		})
	}
}

func TestOverlayAndRoutes(t *testing.T) {
	reg := load(t)
	if _, ok := reg.Providers["antigravity"]; ok {
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

// Ladder entries are opencode sub ids, not registry provider names: known sub ids pass with no providers, others fail.
func TestValidateOpencodeLadderNames(t *testing.T) {
	for _, c := range []struct {
		ladder []string
		want   string
	}{
		{[]string{"opencode-go", "github-copilot", "openai"}, ""},
		{[]string{"openai", "ghost"}, "routing.opencode.ladder names unknown opencode sub id"},
	} {
		reg := &Registry{Routing: Routing{Opencode: OpencodeRouting{Ladder: &c.ladder}}, Providers: map[string]*Provider{}}
		if err := reg.Validate(); (err == nil) != (c.want == "") || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("ladder %v: Validate = %v, want %q", c.ladder, err, c.want)
		}
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
	checkGatewayAuthorize(t, gw)
	t.Setenv("GW_KEY", "from-env")
	if v, _ := Credential(context.Background(), gw); v != "from-env" {
		t.Errorf("env must beat the vault, got %q", v)
	}
	gw.Auth = Auth{Type: AuthKey, Command: "echo minted"}
	if v, _ := Credential(context.Background(), gw); v != "minted" {
		t.Errorf("command cred %q", v)
	}
	sub, _ := reg.Get("ultra")
	h := http.Header{"Authorization": {"Bearer client"}}
	if err := Authorize(context.Background(), sub, h); err != nil || h.Get("Authorization") != "Bearer client" {
		t.Errorf("passthrough must keep client auth: %v %v", err, h)
	}
}

// checkGatewayAuthorize asserts Authorize swaps the client's bearer for the vault key and adds the row's headers.
func checkGatewayAuthorize(t *testing.T, gw *Provider) {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer client"}}
	if err := Authorize(context.Background(), gw, h); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "from-vault" || h.Get("X-Team") != "a" {
		t.Errorf("headers %v", h)
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

// TestAPIKeyAuth: only a harness-held credential (x-api-key or bearer) is a per-token request worth
// recording in the usage ledger — a vendor-CLI passthrough login (subscription) or no auth is not.
func TestAPIKeyAuth(t *testing.T) {
	cases := []struct {
		authType string
		want     bool
	}{
		{AuthKey, true},
		{AuthPAT, true},
		{AuthM2M, true},
		{AuthOAuth, true},
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

func TestSecretName(t *testing.T) {
	cases := []struct{ provider, profile, method, want string }{
		{"databricks", "prod", AuthOAuth, "DATABRICKS_PROD_OAUTH"},
		{"databricks", "dbx-dev.1", AuthM2M, "DATABRICKS_DBX_DEV_1_M2M"},
		{"databricks", "qa", AuthPAT, "DATABRICKS_QA_PAT"},
		{"anthropic", "anthropic", AuthKey, "ANTHROPIC_KEY"}, // collapse: profile == provider
		{"anthropic", "work", AuthKey, "ANTHROPIC_WORK_KEY"}, // no collapse
		{"openai", "openai", AuthKey, "OPENAI_KEY"},          // collapse
		{"github", "personal", AuthOAuth, "GITHUB_PERSONAL_OAUTH"},
		{"anthropic", "ultra", AuthCLI, ""},
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
	if err := vault.Store("ANTHROPIC_API_KEY", "old-key"); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(p) {
		t.Error("old env-named secret must count")
	}
	if err := vault.Store("ANTHROPIC_KEY", "new-key"); err != nil {
		t.Fatal(err)
	}
	if v, err := Credential(t.Context(), p); err != nil || v != "new-key" {
		t.Errorf("Credential = %q, %v; want the new-named secret first", v, err)
	}
}

func TestReadOAuthCorruptSecretIsAnError(t *testing.T) {
	p := mintRow(t, "corrupt-oauth", AuthOAuth, "", "http://127.0.0.1:1")
	if err := vault.Store(AuthSecretName(p), "not-json{"); err != nil {
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
	if err := vault.Store(AuthSecretName(p), "not-json{"); err != nil {
		t.Fatal(err)
	}
	sealAccess(p, "new-access")
	raw, _, err := vault.ReadSecret(AuthSecretName(p))
	if err != nil || raw != "not-json{" {
		t.Fatalf("sealed value = %q, %v; want it untouched after an unreadable read", raw, err)
	}
}

const wireHost = "https://example.cloud.databricks.com"

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// TestRouteAgentsAreTheCanonicalRoster checks RouteAgents is agentid.All() in route order.
func TestRouteAgentsAreTheCanonicalRoster(t *testing.T) {
	got, want := slices.Sorted(slices.Values(RouteAgents)), slices.Sorted(slices.Values(agentid.All()))
	if !slices.Equal(got, want) {
		t.Errorf("sorted RouteAgents = %v, want agentid.All() %v", got, want)
	}
}

// TestDefaultsAgentNamesAreKnown checks every "agent" and auth "login" value in defaults.json is an agentid name.
func TestDefaultsAgentNamesAreKnown(t *testing.T) {
	var doc any
	if err := json.Unmarshal(defaultsJSON, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if name, ok := e.(string); ok && (k == "agent" || k == "login") && !agentid.Known(name) {
					t.Errorf("defaults.json %q = %q is not an agentid name", k, name)
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
}
