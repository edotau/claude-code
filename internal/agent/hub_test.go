package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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

// TestHubLegForOpencodeDefault: the opencode sonnet cell maps to the opencode-go hub leg, argv kimi-k3.1-pro at high.
func TestHubLegForOpencodeDefault(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := hubRegistry()
	tb, err := router.Load()
	if err != nil {
		t.Fatal(err)
	}
	cell := tb.Rows["opencode"]["sonnet"]
	r, model, err := hubLegFor(reg, cell.Model)
	if err != nil {
		t.Fatal(err)
	}
	l := r.(hubLeg)
	_, argv := opencodeRunArgv(l.provider.OpencodeID+"/"+l.resolveModel(model), Headless{Prompt: "p", Effort: cell.Effort})
	got := strings.Join(argv, " ")
	if !strings.Contains(got, "-m opencode-go/kimi-k3.1-pro") || !strings.Contains(got, "--variant high") {
		t.Errorf("opencode default argv = %q, want -m opencode-go/kimi-k3.1-pro --variant high", got)
	}
	if _, _, err := hubLegFor(reg, "ghost/x"); err == nil {
		t.Error("an unknown opencodeID must fail")
	}
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

// hubLeg always execs opencode with the prompt in argv (OpencodeRunArgv), so its own
// MaxPromptBytes check (hub.go, ahead of the binary lookup) stays unconditional, unlike vendorRunner's
// opencode-only gate. This fails before touching paths.VendorBin, so no bin/opencode is needed.
func TestHubLegRunOversizedPromptRefused(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubLeg{agent: "codex", provider: chatgpt, reg: reg}
	big := strings.Repeat("x", MaxPromptBytes+10)

	_, err := l.Run(context.Background(), RunOptions{Prompt: big}, Discard)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(e, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want *Error wrapping ErrPromptTooLarge", err)
	}
}

// TestFirstOutputDeadlineFollowsRegistryWhenNotOverridden checks firstOutputDeadline reads the registry's
// routing.opencode.freeFirstOutput value when the package-level freeFirstOutput override is zero (unset),
// and that the override still wins when a test sets one.
func TestFirstOutputDeadlineFollowsRegistryWhenNotOverridden(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Opencode.FreeFirstOutput = "45s"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := firstOutputDeadline(); got != 45*time.Second {
		t.Errorf("firstOutputDeadline() = %v, want the registry's 45s", got)
	}

	orig := freeFirstOutput
	freeFirstOutput = 10 * time.Second
	t.Cleanup(func() { freeFirstOutput = orig })
	if got := firstOutputDeadline(); got != 10*time.Second {
		t.Errorf("firstOutputDeadline() = %v, want the override (10s) to win over the registry", got)
	}
}

func TestLegLabel(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubLeg{agent: "codex", provider: chatgpt, reg: reg}
	if got := legLabel(l); got != "chatgpt" {
		t.Errorf("hubLeg label = %q, want the provider's name", got)
	}
	if got := legLabel(claudeAgent); got != "claude" {
		t.Errorf("non-hub leg label = %q, want Name()", got)
	}
}

// TestBuildLegPinnedRowWithoutOpencodeID: a pinned Databricks-style row (no opencode id) runs native, not a hub leg.
func TestBuildLegPinnedRowWithoutOpencodeID(t *testing.T) {
	rc := routeCtx{reg: hubRegistry(), want: "opencode"}
	cand := router.Candidate{Agent: "opencode", Profile: "nvidia"}
	leg, skip, err := rc.buildLeg(cand, router.Cell{Model: "nv-opus"})
	if err != nil || skip != "" {
		t.Fatalf("skip=%q err=%v", skip, err)
	}
	if _, hub := leg.Agent.(hubLeg); hub || leg.Model != "nvidia:nv-opus" {
		t.Errorf("want native leg with model nvidia:nv-opus, got %T %q", leg.Agent, leg.Model)
	}
}
