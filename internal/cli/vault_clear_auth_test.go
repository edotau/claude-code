package cli

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault set NAME clears the marks of providers whose sealed secret OR auth.env is NAME, and only those.
func TestClearProviderAuthMatchesSealedSecretAndEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{
		"clr-env":{"kind":"anthropic","base_url":"https://e.example","auth":{"type":"key","header":"x-api-key","env":"CLR_ENV_KEY"},"models":{"opus":"m"}},
		"clr-pat":{"kind":"anthropic","base_url":"https://p.example","auth":{"type":"pat"},"models":{"opus":"m"}},
		"clr-other":{"kind":"anthropic","base_url":"https://o.example","auth":{"type":"key","header":"x-api-key","env":"CLR_OTHER_KEY"},"models":{"opus":"m"}}}}`)
	for _, a := range []string{router.ProviderAccount("clr-env"), "github·clr-env", router.ProviderAccount("clr-pat"), router.ProviderAccount("clr-other")} {
		if err := router.MarkAuth(a, "r"); err != nil {
			t.Fatal(err)
		}
	}
	live := func(a string) bool { _, ok := router.LoadState().AuthMarked(a, time.Now()); return ok }

	clearProviderAuth("CLR_ENV_KEY")
	if live(router.ProviderAccount("clr-env")) || live("github·clr-env") {
		t.Error("vault set of the auth.env name did not clear that provider's marks")
	}
	if !live(router.ProviderAccount("clr-pat")) || !live(router.ProviderAccount("clr-other")) {
		t.Error("clearing one env name touched other providers")
	}

	clearProviderAuth(providers.SecretName(providers.GroupDatabricks, "clr-pat", providers.AuthPAT))
	if live(router.ProviderAccount("clr-pat")) {
		t.Error("vault set of the sealed secret name did not clear its provider")
	}
	if !live(router.ProviderAccount("clr-other")) {
		t.Error("an unrelated mark was cleared")
	}
	clearProviderAuth("UNRELATED_NAME")
	if !live(router.ProviderAccount("clr-other")) {
		t.Error("an unmatched name cleared a mark")
	}
}
