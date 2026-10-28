package providers

import "testing"

func TestProviderIsTheRowsOwnField(t *testing.T) {
	if got := (Provider{Provider: "github"}).provider(); got != "github" {
		t.Errorf("provider() = %q, want github", got)
	}
	if got := (Provider{Protocol: ProtocolOpenAI, BaseURL: "https://api.openai.com/v1"}).provider(); got != "" {
		t.Errorf("a Go-built row without a provider field = %q, want empty", got)
	}
}
