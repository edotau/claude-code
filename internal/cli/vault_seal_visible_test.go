package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// SealSecret (used by login/mint) and `vault set` (secrets.Store) must land the same visible record.
func TestSealSecretVisibleLikeVaultSet(t *testing.T) {
	useVaultTest(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := secrets.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("ZZ_SEALED_SECRET", val); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ZZ_STORE_SECRET", "ZZ_SEALED_SECRET"} {
		if got, src, err := secrets.Lookup(n); err != nil || got != val || src != "vault" {
			t.Errorf("Lookup(%s) = %q (%s) %v", n, got, src, err)
		}
	}
	code, out, _ := captureOutput(t, func() int { return cmdVault([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "ZZ_STORE_SECRET") || !strings.Contains(out, "ZZ_SEALED_SECRET") {
		t.Fatalf("vault list: %d %q", code, out)
	}
	var doc struct {
		Secrets []struct {
			Name, Masked string
			Length       int
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("vault list output not JSON: %v", err)
	}
	got := map[string]string{}
	for _, e := range doc.Secrets {
		got[e.Name] = fmt.Sprintf("%s/%d", e.Masked, e.Length)
	}
	if got["ZZ_STORE_SECRET"] == "" || got["ZZ_STORE_SECRET"] != got["ZZ_SEALED_SECRET"] {
		t.Errorf("vault list renders them differently: %v", got)
	}
}

// vault set NAME clears the marks of providers whose sealed secret OR auth.env is NAME, and only those.
func TestClearProviderAuthMatchesSealedSecretAndEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{
		"clr-env":{"protocol":"anthropic","base_url":"https://e.example","auth":{"type":"key","header":"x-api-key","env":"CLR_ENV_KEY"},"models":{"opus":"m"}},
		"clr-other":{"protocol":"anthropic","base_url":"https://o.example","auth":{"type":"key","header":"x-api-key","env":"CLR_OTHER_KEY"},"models":{"opus":"m"}}},
		"databricks":{
		"clr-pat":{"protocol":"anthropic","base_url":"https://p.example","auth":{"type":"pat"},"models":{"opus":"m"}}}}}`)
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

// A SealSecret write must carry the same metadata prefix, length and audit entry as `vault set` (secrets.Store).
func TestSealSecretMetadataAndAuditMatchStore(t *testing.T) {
	useVaultTest(t)
	const val = "dapi-abcdefghijklmnopqrstuvwxyz0123"
	if err := secrets.Store("ZZ_VIA_STORE", val); err != nil {
		t.Fatal(err)
	}
	if err := providers.SealSecret("ZZ_VIA_SEAL", val); err != nil {
		t.Fatal(err)
	}
	stored, err := secrets.ListStored([]string{"ZZ_VIA_STORE", "ZZ_VIA_SEAL"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]secrets.Stored{}
	for _, s := range stored {
		by[s.Name] = s
	}
	a, b := by["ZZ_VIA_STORE"], by["ZZ_VIA_SEAL"]
	if a.Prefix == "" || a.Prefix != b.Prefix || a.Len != b.Len || a.Len != len(val) {
		t.Errorf("metadata differs: store=%+v seal=%+v", a, b)
	}
	events, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, e := range events {
		if e.Action == audit.SecretSet {
			seen[e.Resource] = e.Detail
		}
		if strings.Contains(e.Resource+e.Detail, val) {
			t.Errorf("audit entry carries the value: %+v", e)
		}
	}
	if seen["ZZ_VIA_STORE"] == "" || seen["ZZ_VIA_SEAL"] != seen["ZZ_VIA_STORE"] {
		t.Errorf("audit entries differ: %v", seen)
	}
}
