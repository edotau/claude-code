package agent

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// hubRegistry builds a minimal registry for hub tests: a ladder of four providers, one without an
// OpencodeID (so HubChain/HubStart must skip or reject it).
func hubRegistry() *providers.Registry {
	mk := func(id string) *providers.Provider {
		return &providers.Provider{Kind: providers.DialectOpenAI, BaseURL: "https://x",
			Models: map[string]string{models.Opus: id + "-opus", models.Sonnet: id + "-sonnet"}, OpencodeID: id}
	}
	reg := &providers.Registry{
		OpencodeLadder: []string{"chatgpt", "github-copilot", "gemini", "nvidia"},
		Providers: map[string]*providers.Provider{
			"chatgpt":        mk("openai"),
			"github-copilot": mk("github-copilot"),
			"gemini":         mk("google"),
			"nvidia":         {Kind: providers.DialectOpenAI, BaseURL: "https://nvidia", Models: map[string]string{models.Opus: "nv-opus"}}, // no OpencodeID
		},
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	return reg
}

func TestRotateLadder(t *testing.T) {
	ladder := []string{"a", "b", "c", "d"}
	if got := rotateLadder(ladder, "b"); !reflect.DeepEqual(got, []string{"b", "c", "d", "a"}) {
		t.Errorf("start in middle: got %v", got)
	}
	if got := rotateLadder(ladder, "zzz"); !reflect.DeepEqual(got, ladder) {
		t.Errorf("absent start must return ladder unchanged: got %v", got)
	}
	if got := rotateLadder(ladder, "a"); !reflect.DeepEqual(got, ladder) {
		t.Errorf("start already first (wrap of zero): got %v", got)
	}
	if got := rotateLadder(ladder, "d"); !reflect.DeepEqual(got, []string{"d", "a", "b", "c"}) {
		t.Errorf("start at end wraps to front: got %v", got)
	}
}

func TestHubChain(t *testing.T) {
	reg := hubRegistry()
	start, err := reg.Get("chatgpt")
	if err != nil {
		t.Fatal(err)
	}

	chain := HubChain(reg, "codex", start, FallbackVendors)
	var names []string
	for _, r := range chain {
		names = append(names, legLabel(r))
	}
	// start ("chatgpt") is skipped; "nvidia" has no OpencodeID so it's skipped too; no claude under FallbackVendors.
	if want := []string{"github-copilot", "gemini"}; !reflect.DeepEqual(names, want) {
		t.Errorf("HubChain(vendors) legs = %v, want %v", names, want)
	}

	all := HubChain(reg, "codex", start, FallbackAll)
	if len(all) != len(chain)+1 {
		t.Fatalf("FallbackAll must append exactly one more leg: got %d, want %d", len(all), len(chain)+1)
	}
	if _, ok := all[len(all)-1].(claudeRunner); !ok {
		t.Errorf("FallbackAll's last leg must be claudeRunner, got %T", all[len(all)-1])
	}

	if got := HubChain(reg, "codex", start, FallbackNone); got != nil {
		t.Errorf("FallbackNone must return nil, got %v", got)
	}
}

func TestHubStart(t *testing.T) {
	reg := hubRegistry()

	// agent default: codex starts at "chatgpt".
	p, err := HubStart(reg, "codex", "")
	if err != nil || p.Name != "chatgpt" {
		t.Errorf("codex default: p=%v err=%v, want chatgpt", p, err)
	}

	// explicit provider overrides the agent default.
	p, err = HubStart(reg, "codex", "gemini")
	if err != nil || p.Name != "gemini" {
		t.Errorf("explicit provider: p=%v err=%v, want gemini", p, err)
	}

	// explicit provider without an OpencodeID errors.
	if _, err := HubStart(reg, "codex", "nvidia"); err == nil {
		t.Error("provider without OpencodeID must error")
	}

	// opencode has no fixed start: falls to the registry's own ladder[0].
	p, err = HubStart(reg, "opencode", "")
	if err != nil || p.Name != reg.OpencodeLadder[0] {
		t.Errorf("opencode default: p=%v err=%v, want ladder[0]=%s", p, err, reg.OpencodeLadder[0])
	}
}

func TestHubLegResolveModel(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubLeg{agent: "codex", provider: chatgpt, reg: reg}

	if got := l.resolveModel(""); got != "openai-opus" {
		t.Errorf(`resolveModel("") = %q, want provider's opus slot`, got)
	}
	if got := l.resolveModel(models.Sonnet); got != "openai-sonnet" {
		t.Errorf("resolveModel(slot name) = %q, want provider's sonnet slot", got)
	}
	if got := l.resolveModel("raw-model-id"); got != "raw-model-id" {
		t.Errorf("resolveModel(raw id) = %q, want it passed through", got)
	}
}

func TestLegLabel(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubLeg{agent: "codex", provider: chatgpt, reg: reg}
	if got := legLabel(l); got != "chatgpt" {
		t.Errorf("hubLeg label = %q, want the provider's name", got)
	}
	if got := legLabel(claudeRunner{}); got != "claude" {
		t.Errorf("non-hub leg label = %q, want Name()", got)
	}
}
