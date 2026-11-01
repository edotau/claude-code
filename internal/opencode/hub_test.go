package opencode

import (
	"context"
	"errors"
	"github.com/edotau/claude-code/internal/agent"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// hubRegistry builds a minimal registry for hub tests: a ladder of four providers, one without an
// OpencodeID (so HubStart must reject it).
func hubRegistry() *providers.Registry {
	mk := func(id string) *providers.Provider {
		return &providers.Provider{Protocol: providers.ProtocolOpenAI, BaseURL: "https://x",
			Models: map[string]string{models.Opus: id + "-opus", models.Sonnet: id + "-sonnet"}, OpencodeID: id}
	}
	reg := &providers.Registry{
		Routing: providers.Routing{Opencode: providers.OpencodeRouting{Ladder: &[]string{"chatgpt", "github-copilot", "gemini", "nvidia"}}},
		Providers: map[string]*providers.Provider{
			"chatgpt":        mk("openai"),
			"github-copilot": mk("github-copilot"),
			"gemini":         mk("google"),
			"nvidia":         {Protocol: providers.ProtocolOpenAI, BaseURL: "https://nvidia", Models: map[string]string{models.Opus: "nv-opus"}}, // no OpencodeID
		},
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	return reg
}


// TestHubStart covers HubStart's login-aware ladder walk through the opencodeLogins seam: an explicit
// provider is forced regardless of login or quota; with no explicit provider, the first opencodeLadder
// entry carrying a login wins, else the ladder's first entry at all (including opencode's own synthetic
// opencode-go provider); an empty ladder errors.
func TestHubStart(t *testing.T) {
	reg := hubRegistry()
	reg.Routing.Opencode.Ladder = &[]string{"opencode-go", "github-copilot", "openai"} // defaults.json order: Go, then Copilot, then ChatGPT
	orig := opencodeLogins
	t.Cleanup(func() { opencodeLogins = orig })
	opencodeLogins = func() []string { return nil }

	// explicit provider is forced regardless of login state — "gemini" (OpencodeID "google") has no login.
	p, err := HubStart(reg, "codex", "gemini")
	if err != nil || p.Name != "gemini" {
		t.Errorf("explicit provider: p=%v err=%v, want gemini (forced, no login needed)", p, err)
	}

	// explicit provider without an OpencodeID errors.
	if _, err := HubStart(reg, "codex", "nvidia"); err == nil {
		t.Error("explicit provider without an OpencodeID must error")
	}

	// no logins anywhere: falls to the ladder's first entry — opencode's own synthetic opencode-go provider.
	p, err = HubStart(reg, "opencode", "")
	if err != nil || p.OpencodeID != "opencode-go" {
		t.Errorf("no logins: p=%v err=%v, want the ladder's first entry opencode-go", p, err)
	}

	// a login further down the ladder wins over the login-less first entry.
	opencodeLogins = func() []string { return []string{"github-copilot"} }
	p, err = HubStart(reg, "opencode", "")
	if err != nil || p.Name != "github-copilot" {
		t.Errorf("github-copilot login: p=%v err=%v, want github-copilot", p, err)
	}

	// opencode's own plan carries its own login id ("opencode", not "opencode-go") and leads the ladder.
	opencodeLogins = func() []string { return []string{"opencode"} }
	p, err = HubStart(reg, "opencode", "")
	if err != nil || p.OpencodeID != "opencode-go" {
		t.Errorf("opencode-go login: p=%v err=%v, want opencode-go", p, err)
	}

	// an empty ladder errors — nothing to fall back to.
	empty := hubRegistry()
	empty.Routing.Opencode.Ladder = nil
	if _, err := HubStart(empty, "opencode", ""); err == nil {
		t.Error("an empty opencodeLadder must error")
	}
}

func TestHubDispatchResolveModel(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubDispatch{agent: "codex", provider: chatgpt, reg: reg}

	if got := l.resolveModel(""); got != "openai-opus" {
		t.Errorf(`resolveModel("") = %q, want provider's opus alias`, got)
	}
	if got := l.resolveModel(models.Sonnet); got != "openai-sonnet" {
		t.Errorf("resolveModel(alias name) = %q, want provider's sonnet alias", got)
	}
	if got := l.resolveModel("raw-model-id"); got != "raw-model-id" {
		t.Errorf("resolveModel(raw id) = %q, want it passed through", got)
	}
}

// hubDispatch always execs opencode with the prompt in argv (OpencodeRunArgv), so its own
// MaxPromptBytes check (hub.go, ahead of the binary lookup) stays unconditional, unlike vendorRunner's
// opencode-only gate. This fails before touching paths.AgentBin, so no bin/opencode is needed.
func TestHubDispatchRunOversizedPromptRefused(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubDispatch{agent: "codex", provider: chatgpt, reg: reg}
	big := strings.Repeat("x", agent.MaxPromptBytes+10)

	_, err := l.Run(context.Background(), agent.RunOptions{Prompt: big}, agent.Discard)
	var e *agent.Error
	if !errors.As(err, &e) || !errors.Is(e, agent.ErrPromptTooLarge) {
		t.Fatalf("err = %v, want *Error wrapping ErrPromptTooLarge", err)
	}
}


