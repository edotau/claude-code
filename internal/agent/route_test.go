package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// codex served by an OpenAI API key bills openai·openai (credits), not the ChatGPT plan's key.
func TestResolveAccountNamesActualBackend(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setPin(t, providers.PinProfile, "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	acct, billing := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "codex:chatgpt"})
	if acct != "openai·openai" || billing != router.BillCredits {
		t.Fatalf("resolveAccount = (%q, %q), want openai·openai credits", acct, billing)
	}
	if a, _ := resolveAccount("opencode", router.Cell{Model: "opencode-go/kimi-k3.1-pro"}); a != "" {
		t.Errorf("opencode keeps its table account, got %q", a)
	}
}

// The session's profile (env) beats the vault pin, and a codex login cell keeps its own account.
func TestResolveAccountFollowsSessionProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setPin(t, providers.PinProfile, "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	t.Setenv(providers.EnvSessionProfile, "openai")
	if acct, _ := resolveAccount("claude", router.Cell{Model: "claude-sonnet-5-5"}); acct != "openai·openai" {
		t.Errorf("claude account = %q, want the session profile openai·openai", acct)
	}
	if acct, _ := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "openai·chatgpt"}); acct != "" {
		t.Errorf("codex login cell account = %q, want it kept (empty override)", acct)
	}
}

// An explicit backend stays one leg, records its own quota failure, and is skipped without an attempt after.
func TestExplicitChainRecordsAndGates(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	codex := &fakeRunner{name: "codex", errs: []error{errors.New("insufficient_quota: you exceeded your current quota")}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "openai"}
	c := ExplicitChain(codex, req)
	if len(c.Legs) != 1 || c.Legs[0].Account != "openai·openai" {
		t.Fatalf("chain = %+v, want one leg on openai·openai", c)
	}
	if _, err := RunWithFallback(context.Background(), c, req, Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["openai·openai"]; e.Status(time.Now()) != router.StateExhausted {
		t.Fatalf("openai·openai not marked: %+v", e)
	}
	again := ExplicitChain(codex, req)
	if len(again.Legs) != 0 || len(again.Skipped) != 1 || !strings.Contains(again.Skipped[0], "openai·openai exhausted") {
		t.Fatalf("second explicit chain = %+v, want a gate skip", again)
	}
	if len(codex.seen) != 1 {
		t.Errorf("attempts = %d, want 1 (the gate stops the second)", len(codex.seen))
	}
}

// --native opencode leads on opencode's own config, ahead of (and instead of) the hub cell.
func TestRouteChainNativeOpencodeLeads(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	all := router.PickOpts{Configured: func(string, router.Cell) error { return nil }}
	c, _, err := RouteChain(context.Background(), "opencode", "sonnet", router.FallbackAll, false, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Legs[0].Agent.(cliRunner); !ok || !c.Legs[0].unroutedLeg {
		t.Fatalf("first leg = %+v, want the native opencode runner, unrouted", c.Legs[0])
	}
	for _, l := range c.Legs[1:] {
		if l.Agent.Name() == "opencode" {
			t.Errorf("the hub opencode cell must not also run: %+v", c.Legs)
		}
	}
	none, _, _ := RouteChain(context.Background(), "opencode", "sonnet", router.FallbackNone, false, true, all)
	if len(none.Legs) != 1 {
		t.Errorf("fallback none = %d legs, want the native leg only", len(none.Legs))
	}
}

// A codex leg keyed on its ChatGPT login that really runs on the session's Databricks profile records and bills that profile.
func TestOwnLoginLegRecordsTheProfileItRan(t *testing.T) {
	gateFallbackEnv(t)
	t.Setenv(providers.EnvSessionProfile, "dbx")
	r := &fakeRunner{name: "codex", errs: []error{nil}}
	c := Chain{Want: "codex", Legs: []Leg{{Agent: r, Account: "openai·chatgpt", Billing: router.BillSubscription}}}
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || len(res.Hops) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := res.Hops[0].Account; got != "databricks·dbx" {
		t.Errorf("hop account = %q, want databricks·dbx (what the runner resolves)", got)
	}
}

// Skip notices name only the wanted agent's gates; other agents' reasons are not printed.
func TestSkipNoticeOnlyForWantedAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c := Single(&fakeRunner{name: "copilot", errs: []error{nil}})
	c.Skipped = []string{"opencode·openai: not configured", "copilot: exhausted"}
	var got []string
	if _, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Contains(got[0], "opencode") || !strings.Contains(got[0], "copilot: exhausted") {
		t.Errorf("notices = %q, want only the copilot skip", got)
	}
}
