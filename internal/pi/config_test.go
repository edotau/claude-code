package pi

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/catalog"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type piModel struct {
	ID            string         `json:"id"`
	Reasoning     bool           `json:"reasoning"`
	ContextWindow int            `json:"contextWindow"`
	MaxTokens     int            `json:"maxTokens"`
	Compat        map[string]any `json:"compat"`
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
		{"unknown model gets the default window", pr["dbx-chat"].Models[2].ID == "databricks-unlisted-x" && pr["dbx-chat"].Models[2].ContextWindow == models.ContextWindowUnknown},
		{"defaults", strings.Contains(sj, `"defaultProvider": "dbx-claude"`)},
	}
}

func compatChecks(pr map[string]piProv) []buildCheck {
	return []buildCheck{
		{"chat non-GPT gets max_tokens and a capped limit", pr["dbx-chat"].Models[2].Compat["maxTokensField"] == "max_tokens" && pr["dbx-chat"].Models[2].MaxTokens <= 8192},
		{"GPT chat keeps max_completion_tokens", compatFor("databricks-gpt-5-4-mini", providers.ProtocolOpenAI) == nil},
		{"claude compat unchanged", len(pr["dbx-claude"].Models[0].Compat) == 1 && pr["dbx-claude"].Models[0].Compat["forceAdaptiveThinking"] == true},
	}
}

func TestBuildConfig(t *testing.T) {
	p := testProvider(t)
	full := Bases{Claude: "http://127.0.0.1:1/p/dbx", Responses: "http://127.0.0.1:1/p/dbx/responses", Chat: "http://127.0.0.1:1/p/dbx/openai"}
	mj, sj, menu, err := BuildConfig(p, p, full, "databricks-claude-sonnet-5-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out piOut
	if err := json.Unmarshal(mj, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range append(buildChecks(out.Providers, full, menu, string(mj), string(sj)), compatChecks(out.Providers)...) {
		if !c.ok {
			t.Errorf("%s: models=%s settings=%s", c.name, mj, sj)
		}
	}
	checkLaunchedWindow(t, p, full, out)
}

// a launched window (typed [1m]) reaches only the run's model entry; untyped (0) and other entries keep the menu rule
func checkLaunchedWindow(t *testing.T, p *providers.Provider, full Bases, out piOut) {
	t.Helper()
	const launched = 777_000
	typed, _, _, err := BuildConfig(p, p, full, "databricks-claude-sonnet-5-5", launched)
	if err != nil {
		t.Fatal(err)
	}
	var tout piOut
	if err := json.Unmarshal(typed, &tout); err != nil {
		t.Fatal(err)
	}
	win := func(o piOut, prov, id string) int {
		for _, m := range o.Providers[prov].Models {
			if m.ID == id {
				return m.ContextWindow
			}
		}
		return -1
	}
	if got := win(tout, "dbx-claude", "databricks-claude-sonnet-5-5"); got != launched || win(out, "dbx-claude", "databricks-claude-sonnet-5-5") == launched {
		t.Errorf("launched entry window = %d, want %d (untyped %d)", got, launched, win(out, "dbx-claude", "databricks-claude-sonnet-5-5"))
	}
	if win(tout, "dbx-chat", "databricks-kimi-k3") != win(out, "dbx-chat", "databricks-kimi-k3") {
		t.Error("a launched window changed another menu entry")
	}
}

func TestBuildConfigRoutelessWire(t *testing.T) {
	p := testProvider(t)
	mj, _, menu, err := BuildConfig(p, p, Bases{Claude: "http://127.0.0.1:1/p/dbx", Chat: "http://127.0.0.1:1/p/dbx/openai"}, "", 0)
	if err != nil || strings.Contains(string(mj), "dbx-gpt") || menu["databricks-gpt-5-4-mini"] != "" {
		t.Errorf("routeless responses wire kept models: err=%v menu=%v", err, menu)
	}
}

func TestPickWireDropsResponsesOnlyWithoutRoute(t *testing.T) {
	p := testProvider(t)
	delete(p.Routes, providers.ProtocolResponses)
	if w, ok := pickWire(p, "databricks-gpt-6-1-sol"); ok || w != providers.ProtocolResponses {
		t.Errorf("responses-only model kept on %q (ok=%v), want dropped", w, ok)
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

func TestBuildConfigDefaultIsTheRunsModelElseFirstEntry(t *testing.T) {
	p := testProvider(t)
	bases := Bases{Claude: "http://127.0.0.1:1/p/dbx", Responses: "http://127.0.0.1:1/p/dbx/responses", Chat: "http://127.0.0.1:1/p/dbx/openai"}
	for want, def := range map[string]string{"databricks-kimi-k3": "databricks-kimi-k3", "not-on-the-menu": ""} {
		_, sj, menu, err := BuildConfig(p, p, bases, want, 0)
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		var s struct{ DefaultProvider, DefaultModel string }
		if err := json.Unmarshal(sj, &s); err != nil || menu[s.DefaultModel] != s.DefaultProvider || s.DefaultModel == "" {
			t.Fatalf("%s: settings %s not on the menu: %v", want, sj, err)
		}
		if def != "" && s.DefaultModel != def {
			t.Errorf("default %q, want the run's model %q", s.DefaultModel, def)
		}
	}
}

func TestRunDirNamesItsOwner(t *testing.T) {
	vaulttest.Use(t)
	dir, rm, err := NewRunDir("dbx")
	if err != nil {
		t.Fatal(err)
	}
	defer rm()
	if want := RunDirPrefix + strconv.Itoa(os.Getpid()) + "-"; !strings.HasPrefix(filepath.Base(dir), want) {
		t.Errorf("run dir %s does not start with %s", dir, want)
	}
}

// The pool predicate is the menu BuildConfig writes: one id from each wire runs, a Gemini id does not.
func TestRunnableIsTheMenu(t *testing.T) {
	p := testProvider(t)
	for _, id := range []string{"databricks-claude-sonnet-5-5", "databricks-gpt-5-4-mini", "databricks-kimi-k3"} {
		if !(piEngine{}).Runnable(p, id) {
			t.Errorf("%s should be runnable", id)
		}
	}
	if (piEngine{}).Runnable(p, "databricks-gemini-4-argon") {
		t.Error("gemini is not on Pi's menu")
	}
}

// A routed copy has other URLs, so its scope misses the picker additions: the menu is read from the configured profile.
func TestBuildConfigMenuReadsTheConfiguredProfile(t *testing.T) {
	src := testProvider(t)
	if _, err := catalog.AppendPickerModels(src, []providers.ModelInfo{{ID: "databricks-gemini-4-argon", Ready: "READY", Task: "llm/v1/chat"}}); err != nil {
		t.Fatal(err)
	}
	routed := *src
	routed.BaseURL = "http://127.0.0.1:1/p/dbx"
	bases := Bases{Claude: "x", Responses: "x", Chat: "x"}
	if _, _, menu, err := BuildConfig(&routed, src, bases, "", 0); err != nil || menu["databricks-gemini-4-argon"] != "dbx-chat" {
		t.Fatalf("menu[gemini] = %q, err %v; want dbx-chat", menu["databricks-gemini-4-argon"], err)
	}
	if _, _, menu, _ := BuildConfig(&routed, &routed, bases, "", 0); menu["databricks-gemini-4-argon"] != "" {
		t.Fatal("control: the routed copy should not see the picker addition")
	}
	if !(piEngine{}).Runnable(src, "databricks-gemini-4-argon") {
		t.Fatal("pool predicate must agree with the menu")
	}
}
