package configure

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestBillingLabel(t *testing.T) {
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := billingLabel(reg, "max"); got != "subscription" {
		t.Errorf("billingLabel(max) = %q", got)
	}
	if got := billingLabel(reg, "anthropic"); got != "API key" {
		t.Errorf("billingLabel(anthropic) = %q", got)
	}
	if got := billingLabel(reg, "chatgpt"); got != "chatgpt (not anthropic)" {
		t.Errorf("billingLabel(chatgpt) = %q", got)
	}
}
