package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vendors"
)

// hubRegistry builds a minimal registry for hub tests: a ladder of four providers, one without an
// OpencodeID (so HubStart must reject it).
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

// TestHubLegForOpencodeDefault: the opencode sonnet cell maps to the opencode-go hub leg, argv kimi-k3.1-pro at high.
func TestHubLegForOpencodeDefault(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := hubRegistry()
	tb, err := route.Load()
	if err != nil {
		t.Fatal(err)
	}
	cell := tb.Rows["opencode"]["sonnet"]
	r, model, err := hubLegFor(reg, cell.Model)
	if err != nil {
		t.Fatal(err)
	}
	l := r.(hubLeg)
	_, argv := vendors.OpencodeRunArgv(l.provider.OpencodeID+"/"+l.resolveModel(model), vendors.Headless{Prompt: "p", Effort: cell.Effort})
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "-m opencode-go/kimi-k3.1-pro") || !strings.Contains(got, "--variant high") {
		t.Errorf("opencode default argv = %q, want -m opencode-go/kimi-k3.1-pro --variant high", got)
	}
	if _, _, err := hubLegFor(reg, "ghost/x"); err == nil {
		t.Error("an unknown opencodeID must fail")
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

// hubLeg always execs opencode with the prompt in argv (vendors.OpencodeRunArgv), so its own
// MaxPromptBytes check (hub.go, ahead of the binary lookup) stays unconditional, unlike vendorRunner's
// opencode-only gate. This fails before touching paths.VendorBin, so no bin/opencode is needed.
func TestHubLegRunOversizedPromptRefused(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubLeg{agent: "codex", provider: chatgpt, reg: reg}
	big := strings.Repeat("x", MaxPromptBytes+10)

	_, err := l.Run(context.Background(), Request{Prompt: big}, io.Discard)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(e, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want *Error wrapping ErrPromptTooLarge", err)
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
