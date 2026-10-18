package configure

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// pickerNames must put anthropic first and fold subscription out of the list entirely (it is a
// billing mode of the anthropic row, not a separate picker entry).
func TestPickerNamesAnthropicFirstSubscriptionFolded(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"anthropic":    {},
		"subscription": {},
		"openai":       {},
		"gemini":       {},
	}}
	got := pickerNames(reg)
	if len(got) != 3 || got[0] != "anthropic" {
		t.Fatalf("pickerNames = %v, want anthropic first and exactly 3 entries", got)
	}
	for _, n := range got {
		if n == "subscription" {
			t.Fatalf("pickerNames must fold subscription out: %v", got)
		}
	}
}

// pickerNames on a registry with no anthropic provider must not synthesize one.
func TestPickerNamesNoAnthropic(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"openai": {}}}
	got := pickerNames(reg)
	if len(got) != 1 || got[0] != "openai" {
		t.Fatalf("pickerNames = %v, want [openai]", got)
	}
}

func TestBillingLabel(t *testing.T) {
	if got := billingLabel("subscription"); got != "subscription" {
		t.Errorf("billingLabel(subscription) = %q", got)
	}
	if got := billingLabel("anthropic"); got != "api key" {
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
