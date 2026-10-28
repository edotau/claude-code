package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func kindRow(kind, clientID, _ string) *Provider {
	if kind == "" {
		kind = AuthKey
	}
	return &Provider{Name: "dbx-test", Provider: GroupDatabricks, Kind: DialectAnthropic, BaseURL: "https://example.cloud.databricks.com/serving-endpoints/anthropic",
		Auth: Auth{Type: kind, ClientID: clientID}}
}

func TestValidateKind(t *testing.T) {
	cases := []struct {
		name string
		p    *Provider
		want string
	}{
		{"none", kindRow("", "", AuthKey), ""},
		{"u2m", kindRow(AuthOAuth, "", AuthKey), ""},
		{"m2m ok", kindRow(AuthM2M, "cid", AuthKey), ""},
		{"m2m no client", kindRow(AuthM2M, "", AuthKey), "needs auth.clientId"},
		{"stray clientId", kindRow(AuthPAT, "cid", ""), "needs auth.type m2m"},
	}
	for _, c := range cases {
		errs := strings.Join(validateAuthType("x", c.p), ";")
		if c.want == "" && errs != "" || c.want != "" && !strings.Contains(errs, c.want) {
			t.Errorf("%s: errs %q, want %q", c.name, errs, c.want)
		}
	}
}

func TestKindSecretName(t *testing.T) {
	p := kindRow(AuthOAuth, "", AuthKey)
	p.Name = "dbx-dev.1"
	if got := AuthSecretName(p); got != "DATABRICKS_DBX_DEV_1_OAUTH" {
		t.Errorf("got %q", got)
	}
	if AuthSecretName(kindRow("", "", AuthKey)) != "" {
		t.Error("kindless row has a sealed name")
	}
}

func TestPATCredentialAndPresence(t *testing.T) {
	vaulttest.Use(t) // sealing needs the vault master key; Linux CI has no keyring
	p := kindRow(AuthPAT, "", AuthKey)
	if CredentialPresent(p) {
		t.Fatal("present before sealing")
	}
	if err := SealSecret(AuthSecretName(p), "dapi-test-value"); err != nil {
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
	row := map[string]any{"auth": map[string]any{"type":"pat"}}
	if err := SetProviderFields("openai", row); err != nil {
		t.Fatal(err)
	}
	row["auth"] = map[string]any{"type":"oauth"}
	err := SetProviderFields("openai", row)
	if err == nil || !strings.Contains(err.Error(), "refused without force") {
		t.Fatalf("kind change err = %v", err)
	}
	if err := SetProviderFields("openai", map[string]any{"base_url": "https://a.example/v1"}); err != nil {
		t.Errorf("same-kind write refused: %v", err)
	}
}
