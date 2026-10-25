package providers

import (
	"context"
	"strings"
	"testing"
)

func kindRow(kind, clientID, typ string) *Provider {
	return &Provider{Name: "dbx-test", Kind: DialectAnthropic, BaseURL: "https://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: typ, AuthType: kind, ClientID: clientID}}
}

func TestValidateKind(t *testing.T) {
	cases := []struct {
		name string
		p    *Provider
		want string
	}{
		{"none", kindRow("", "", AuthBearer), ""},
		{"u2m", kindRow(AuthU2M, "", AuthBearer), ""},
		{"m2m ok", kindRow(AuthOAuthM2M, "cid", AuthBearer), ""},
		{"m2m no client", kindRow(AuthOAuthM2M, "", AuthBearer), "needs auth.clientId"},
		{"unknown", kindRow("saml", "", AuthBearer), "not supported (supported: u2m, oauth-m2m, pat)"},
		{"non-bearer", kindRow(AuthPAT, "", AuthAPIKey), "needs auth.type bearer"},
		{"stray clientId", kindRow(AuthPAT, "cid", AuthBearer), "needs auth.authType oauth-m2m"},
	}
	for _, c := range cases {
		errs := strings.Join(validateAuthType("x", c.p), ";")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("%s: errs %q, want %q", c.name, errs, c.want)
		}
	}
}

func TestKindSecretName(t *testing.T) {
	p := kindRow(AuthU2M, "", AuthBearer)
	p.Name = "dbx-dev.1"
	if got := authSecretName(p); got != "PROVIDER_DBX_DEV_1_REFRESH_TOKEN" {
		t.Errorf("got %q", got)
	}
	if authSecretName(kindRow("", "", AuthBearer)) != "" {
		t.Error("kindless row has a sealed name")
	}
}

func TestPATCredentialAndPresence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := kindRow(AuthPAT, "", AuthBearer)
	if CredentialPresent(p) {
		t.Fatal("present before sealing")
	}
	if err := SealSecret(authSecretName(p), "dapi-test-value"); err != nil {
		t.Fatal(err)
	}
	if !CredentialPresent(p) {
		t.Error("absent after sealing")
	}
	got, err := Credential(context.Background(), p)
	if err != nil || got != "dapi-test-value" {
		t.Errorf("Credential = %q, %v", got, err)
	}
}

func TestKindChangeRefusedWithoutForce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	row := map[string]any{"auth": map[string]any{"type": "bearer", "authType": "pat"}}
	if err := SetProviderFields("openai", row); err != nil {
		t.Fatal(err)
	}
	row["auth"] = map[string]any{"type": "bearer", "authType": "u2m"}
	err := SetProviderFields("openai", row)
	if err == nil || !strings.Contains(err.Error(), "refused without force") {
		t.Fatalf("kind change err = %v", err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://a.example/v1"}); err != nil {
		t.Errorf("same-kind write refused: %v", err)
	}
}
