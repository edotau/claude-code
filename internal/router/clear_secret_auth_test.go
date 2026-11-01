package router

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault set NAME clears the marks of providers whose sealed secret OR auth.env is NAME, and only those.
func TestClearProviderAuthMatchesSealedSecretAndEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{
		"clr-env":{"protocol":"anthropic","base_url":"https://e.example","auth":{"type":"key","header":"x-api-key","env":"CLR_ENV_KEY"},"models":{"opus":"m"}},
		"clr-other":{"protocol":"anthropic","base_url":"https://o.example","auth":{"type":"key","header":"x-api-key","env":"CLR_OTHER_KEY"},"models":{"opus":"m"}}},
		"databricks":{
		"clr-pat":{"protocol":"anthropic","base_url":"https://p.example","auth":{"type":"pat"},"models":{"opus":"m"}}}}}`)
	for _, a := range []string{ProviderAccount("clr-env"), "github·clr-env", ProviderAccount("clr-pat"), ProviderAccount("clr-other")} {
		if err := MarkAuth(a, "r"); err != nil {
			t.Fatal(err)
		}
	}
	live := func(a string) bool { _, ok := LoadState().AuthMarked(a, time.Now()); return ok }

	ClearSecretAuth("CLR_ENV_KEY")
	if live(ProviderAccount("clr-env")) || live("github·clr-env") {
		t.Error("vault set of the auth.env name did not clear that provider's marks")
	}
	if !live(ProviderAccount("clr-pat")) || !live(ProviderAccount("clr-other")) {
		t.Error("clearing one env name touched other providers")
	}

	ClearSecretAuth(providers.SecretName(providers.GroupDatabricks, "clr-pat", providers.AuthPAT))
	if live(ProviderAccount("clr-pat")) {
		t.Error("vault set of the sealed secret name did not clear its provider")
	}
	if !live(ProviderAccount("clr-other")) {
		t.Error("an unrelated mark was cleared")
	}
	ClearSecretAuth("UNRELATED_NAME")
	if !live(ProviderAccount("clr-other")) {
		t.Error("an unmatched name cleared a mark")
	}
}
