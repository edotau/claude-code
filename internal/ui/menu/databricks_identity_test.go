package menu

import (
	"context"
	"fmt"
	"testing"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
)

func TestDbxIdentityPromptCancellationKeepsStoredPair(t *testing.T) {
	for _, cancelAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("prompt-%d", cancelAt), func(t *testing.T) {
			d := dbxEnv(t)
			if err := databricks.SaveIdentity(d.reg, "dev", providers.IdentityAuto, "old-client", "stored-VALUE"); err != nil {
				t.Fatal(err)
			}
			d.reload()
			f := &dbxFake{}
			installDbxFake(t, f)
			prompts := 0
			dbxSecret = func(string, string, func(string) string) (string, error) {
				prompts++
				if prompts == cancelAt {
					return "", context.Canceled
				}
				return "replacement-client", nil
			}
			dbxIdentityPrompt(d, "dev", providers.IdentitySP, d.reg.Providers["dev"], "stored-VALUE")
			if prompts != cancelAt || len(f.calls) != 0 || d.changed {
				t.Fatalf("cancel performed work: prompts=%d calls=%v changed=%v", prompts, f.calls, d.changed)
			}
			d.reload()
			if got := d.reg.Providers["dev"]; got.Identity != providers.IdentityAuto || got.Auth.SPClientID != "old-client" {
				t.Errorf("identity=%q client=%q after cancellation", got.Identity, got.Auth.SPClientID)
			}
			assertStoredSecret(t, providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M), "stored-VALUE")
		})
	}
}
