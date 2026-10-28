package providers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

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
		if err := json.Unmarshal([]byte(`{"kind":"`+c.kind+`","auth":`+c.auth+`}`), &p); err != nil {
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
		if err := json.Unmarshal([]byte(`{"kind":"openai","auth":`+auth+`}`), &p); err != nil || !p.APIKeyAuth() {
			t.Errorf("%s: APIKeyAuth = %v, %v", auth, p.APIKeyAuth(), err)
		}
	}
	for _, auth := range []string{`{"type":"cli"}`, `{"type":"none"}`} {
		var p Provider
		_ = json.Unmarshal([]byte(`{"kind":"openai","auth":`+auth+`}`), &p)
		if p.APIKeyAuth() {
			t.Errorf("%s: APIKeyAuth must be false", auth)
		}
	}
}

func TestPinNames(t *testing.T) {
	for got, want := range map[string]string{
		PinProfile: "CLAUDE_PROFILE", PinModel: "CLAUDE_MODEL", SlotPin("opus"): "CLAUDE_OPUS_MODEL",
		PinCopilotProfile: "COPILOT_PROFILE", PinCopilotContext: "COPILOT_CONTEXT", PinCodexEffort: "CODEX_EFFORT",
		PinCopilotEffort: "COPILOT_EFFORT", PinGeminiModel: "GEMINI_MODEL", PinCodexModel: "CODEX_MODEL",
		PinCopilotModel: "COPILOT_MODEL", PinCodexProfile: "CODEX_PROFILE", PinGeminiProfile: "GEMINI_PROFILE",
		PinOpencodeProfile: "OPENCODE_PROFILE",
	} {
		if got != want {
			t.Errorf("pin %q, want %q", got, want)
		}
	}
}

func TestSessionProfileIsAnEnvMarkerNotAPin(t *testing.T) {
	load(t)
	t.Setenv(EnvSessionProfile, "")
	writeProviderPins(t, map[string]string{PinProfile: "dev"})
	if SessionProfile() != "" {
		t.Error("the vault pin leaked into SessionProfile")
	}
	t.Setenv(EnvSessionProfile, " qa ")
	if SessionProfile() != "qa" {
		t.Errorf("SessionProfile = %q", SessionProfile())
	}
	if got := Pin(PinCodexModel); got != "" {
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
	p := &Provider{Name: "anthropic", Provider: "anthropic", Kind: DialectAnthropic, Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "ANTHROPIC_API_KEY"}}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if CredentialPresent(p) {
		t.Fatal("present before any secret")
	}
	if err := SealSecret("ANTHROPIC_API_KEY", "old-key"); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(p) {
		t.Error("old env-named secret must count")
	}
	if err := SealSecret("ANTHROPIC_KEY", "new-key"); err != nil {
		t.Fatal(err)
	}
	if v, err := Credential(t.Context(), p); err != nil || v != "new-key" {
		t.Errorf("Credential = %q, %v; want the new-named secret first", v, err)
	}
}
