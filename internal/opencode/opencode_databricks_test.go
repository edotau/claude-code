package opencode

import (
	"github.com/edotau/claude-code/internal/agent"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

type ocProv struct {
	NPM     string `json:"npm"`
	Options struct {
		BaseURL string            `json:"baseURL"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers"`
	} `json:"options"`
	Models map[string]struct {
		Name    string `json:"name"`
		Limit   any    `json:"limit"`
		Options struct {
			ToolStreaming *bool `json:"toolStreaming"`
		} `json:"options"`
	} `json:"models"`
}

type ocCfg struct {
	Model    string            `json:"model"`
	Provider map[string]ocProv `json:"provider"`
}


func dbxRow(name, prefix string) *providers.Provider {
	host := "https://h.example.com"
	return &providers.Provider{
		Name: name, Preset: providers.PresetDatabricks, ModelPrefix: prefix,
		Routes: map[string]string{providers.ProtocolOpenAI: host + "/serving-endpoints", providers.ProtocolResponses: host + "/serving-endpoints"},
	}
}

func checkDbxMenu(t *testing.T, c ocCfg, row, prefix string) {
	t.Helper()
	chat, gpt := c.Provider["harness-"+row], c.Provider["harness-"+row+"-gpt"]
	if chat.NPM != "@ai-sdk/openai-compatible" || gpt.NPM != "@ai-sdk/openai" {
		t.Errorf("npm = %q / %q", chat.NPM, gpt.NPM)
	}
	if gpt.Options.APIKey != "{env:"+agent.OpenCodeKeyEnv+"}" || chat.Options.APIKey != gpt.Options.APIKey {
		t.Errorf("apiKey refs = %q / %q", chat.Options.APIKey, gpt.Options.APIKey)
	}
	claude, hasClaude := c.Provider["harness-"+row+"-claude"]
	total := len(chat.Models) + len(gpt.Models) + len(claude.Models)
	if total != len(providers.DatabricksOpencodeMenu) || len(gpt.Models) != 3 {
		t.Errorf("models chat=%d gpt=%d claude=%d, menu=%d", len(chat.Models), len(gpt.Models), len(claude.Models), len(providers.DatabricksOpencodeMenu))
	}
	if hasClaude && (claude.NPM != "@ai-sdk/anthropic" || claude.Options.APIKey != gpt.Options.APIKey) {
		t.Errorf("claude provider npm=%q apiKey=%q", claude.NPM, claude.Options.APIKey)
	}
	for _, m := range providers.DatabricksOpencodeMenu {
		set := chat
		switch {
		case m.Responses:
			set = gpt
		case hasClaude && models.FamilyOf(m.ID) != "":
			set = claude
		}
		e, ok := set.Models[prefix+m.ID]
		if !ok {
			t.Errorf("missing %q (responses=%v)", prefix+m.ID, m.Responses)
			continue
		}
		if (set.NPM == "@ai-sdk/anthropic") != (e.Options.ToolStreaming != nil && !*e.Options.ToolStreaming) || (e.Options.ToolStreaming != nil && set.NPM != "@ai-sdk/anthropic") {
			t.Errorf("%s toolStreaming=%v on %s", m.ID, e.Options.ToolStreaming, set.NPM)
		}
		if e.Limit != nil {
			t.Errorf("%s carries a limit: %v", m.ID, e.Limit)
		}
		if !strings.HasSuffix(e.Name, " · "+row) || !strings.HasPrefix(e.Name, m.Label) {
			t.Errorf("%s name = %q", m.ID, e.Name)
		}
	}
	for _, set := range []ocProv{chat, gpt, claude} {
		for id := range set.Models {
			if !strings.HasPrefix(id, prefix) {
				t.Errorf("id %q lacks prefix %q", id, prefix)
			}
		}
	}
}









