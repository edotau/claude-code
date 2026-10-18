package configure

import (
	"testing"
)

func TestBillingLabel(t *testing.T) {
	if got := billingLabel("subscription"); got != "subscription" {
		t.Errorf("billingLabel(subscription) = %q", got)
	}
	if got := billingLabel("anthropic"); got != "API key" {
		t.Errorf("billingLabel(anthropic) = %q", got)
	}
	if got := billingLabel("openai"); got != "openai (not anthropic)" {
		t.Errorf("billingLabel(openai) = %q", got)
	}
}

func TestIsAnthropicBilling(t *testing.T) {
	for name, want := range map[string]bool{"anthropic": true, "subscription": true, "openai": false} {
		if got := isAnthropicBilling(name); got != want {
			t.Errorf("isAnthropicBilling(%q) = %v, want %v", name, got, want)
		}
	}
}
