package pi

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type piModel struct {
	ID            string `json:"id"`
	Reasoning     bool   `json:"reasoning"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
}

type piOut struct {
	Providers map[string]piProv `json:"providers"`
}

type piProv struct {
	BaseURL string            `json:"baseUrl"`
	API     string            `json:"api"`
	APIKey  string            `json:"apiKey"`
	Headers map[string]string `json:"headers"`
	Models  []piModel         `json:"models"`
	Compat  map[string]any    `json:"compat"`
}

func testProvider(t *testing.T) *providers.Provider {
	t.Helper()
	vaulttest.Use(t)
	old := agentconfig.SetCatalogWarn(io.Discard)
	t.Cleanup(func() { agentconfig.SetCatalogWarn(old) })
	menu := []providers.OpencodeModel{
		{ID: "claude-sonnet-5-5", Label: "Sonnet"}, {ID: "gpt-5-4-mini", Label: "Mini"},
		{ID: "grok-4-7", Label: "Grok", Responses: true}, {ID: "kimi-k3", Label: "Kimi"}, {ID: "databricks-unlisted-x", Label: "Unk"},
	}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	return &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, Models: map[string]string{"sonnet": "claude-sonnet-5-5"},
		Headers: map[string]string{"X-Claude-Code-Agent": "pi"}, BaseURL: "https://workspace.example.test", Protocol: providers.ProtocolOpenAI,
		Routes: map[string]string{providers.ProtocolAnthropic: "anthropic", providers.ProtocolResponses: "openai/v1"}}
}

func ids(ms []piModel) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.ID)
	}
	return strings.Join(s, ",")
}

type buildCheck struct {
	name string
	ok   bool
}

func buildChecks(pr map[string]piProv, full Bases, menu map[string]string, mj, sj string) []buildCheck {
	return []buildCheck{
		{"three apis", pr["dbx-claude"].API == "anthropic-messages" && pr["dbx-gpt"].API == "openai-responses" && pr["dbx-chat"].API == "openai-completions"},
		{"bare claude base", pr["dbx-claude"].BaseURL == full.Claude},
		{"responses and chat bases", pr["dbx-gpt"].BaseURL == full.Responses && pr["dbx-chat"].BaseURL == full.Chat},
		{"key ref and header", pr["dbx-chat"].APIKey == "$HARNESS_PROXY_API_KEY" && pr["dbx-chat"].Headers["X-Claude-Code-Agent"] == "pi"},
		{"gpt on responses, claude on anthropic", menu["databricks-gpt-5-4-mini"] == "dbx-gpt" && menu["databricks-claude-sonnet-5-5"] == "dbx-claude"},
		{"open model on chat without store", menu["databricks-kimi-k3"] == "dbx-chat" && pr["dbx-chat"].Compat["supportsStore"] == false},
		{"grok on chat", menu["databricks-grok-4-7"] == "dbx-chat" && !strings.Contains(ids(pr["dbx-gpt"].Models), "grok")},
		{"known limits", pr["dbx-claude"].Models[0].ContextWindow > 0 && pr["dbx-claude"].Models[0].MaxTokens > 0},
		{"unknown model has no limits", pr["dbx-chat"].Models[2].ID == "databricks-unlisted-x" && pr["dbx-chat"].Models[2].ContextWindow == 0},
		{"defaults", strings.Contains(sj, `"defaultProvider": "dbx-claude"`)},
	}
}

func TestBuildConfig(t *testing.T) {
	p := testProvider(t)
	full := Bases{Claude: "http://127.0.0.1:1/p/dbx", Responses: "http://127.0.0.1:1/p/dbx/responses", Chat: "http://127.0.0.1:1/p/dbx/openai"}
	mj, sj, menu, err := BuildConfig(p, full)
	if err != nil {
		t.Fatal(err)
	}
	var out piOut
	if err := json.Unmarshal(mj, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range buildChecks(out.Providers, full, menu, string(mj), string(sj)) {
		if !c.ok {
			t.Errorf("%s: models=%s settings=%s", c.name, mj, sj)
		}
	}
}

func TestBuildConfigRoutelessWire(t *testing.T) {
	p := testProvider(t)
	mj, _, menu, err := BuildConfig(p, Bases{Claude: "http://127.0.0.1:1/p/dbx", Chat: "http://127.0.0.1:1/p/dbx/openai"})
	if err != nil || strings.Contains(string(mj), "dbx-gpt") || menu["databricks-gpt-5-4-mini"] != "" {
		t.Errorf("routeless responses wire kept models: err=%v menu=%v", err, menu)
	}
}

func TestRunDirsAreIsolated(t *testing.T) {
	vaulttest.Use(t)
	a, rmA, err := NewRunDir("dbx")
	if err != nil {
		t.Fatal(err)
	}
	b, rmB, err := NewRunDir("dbx")
	if err != nil || a == b {
		t.Fatalf("two dispatches share a dir: %s %s %v", a, b, err)
	}
	rmA()
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s", a)
	}
	if _, err := os.Stat(b); err != nil {
		t.Errorf("cleanup of one run removed the other: %v", err)
	}
	rmB()
}

func TestRowIsReadOnlyOnly(t *testing.T) {
	if !row.CLI.ReadOnlyOnly {
		t.Error("pi must stay out of editing dispatches' fallbacks until the stage 2 guard exists")
	}
}
