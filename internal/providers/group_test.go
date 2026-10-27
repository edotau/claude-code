package providers

import "testing"

func TestGroupBuiltinRows(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"anthropic": "anthropic", "subscription": "anthropic", "openai": "openai", "chatgpt": "openai",
		"github-copilot": "github", "gemini": "google", "nvidia": "opensource", "ollama": "opensource",
	} {
		p := reg.Providers[name]
		if p == nil {
			t.Fatalf("built-in row %q missing", name)
		}
		if got := Group(p); got != want {
			t.Errorf("Group(%s) = %q, want %q", name, got, want)
		}
	}
	if got := Group(&Provider{Preset: PresetDatabricks}); got != GroupDatabricks {
		t.Errorf("databricks preset = %q", got)
	}
	if got := Group(&Provider{Auth: Auth{AuthType: "u2m"}}); got != GroupDatabricks {
		t.Errorf("sealed authType = %q", got)
	}
}
