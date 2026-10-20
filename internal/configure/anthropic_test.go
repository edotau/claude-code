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
