package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/route"
)

// codex served by an OpenAI API key bills codex:openai (credits), not the ChatGPT plan's key.
func TestResolveAccountNamesActualBackend(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	acct, billing := resolveAccount("codex", route.Cell{Model: "gpt-6.1-sol", Account: "codex:chatgpt"})
	if acct != "codex:openai" || billing != route.BillCredits {
		t.Fatalf("resolveAccount = (%q, %q), want codex:openai credits", acct, billing)
	}
	if a, _ := resolveAccount("opencode", route.Cell{Model: "opencode-go/kimi-k3.1-pro"}); a != "" {
		t.Errorf("opencode keeps its table account, got %q", a)
	}
}

// An explicit backend stays one leg, records its own quota failure, and is skipped without an attempt after.
func TestExplicitChainRecordsAndGates(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	codex := &fakeRunner{name: "codex", errs: []error{errors.New("insufficient_quota: you exceeded your current quota")}, emptyAnswer: true}
	req := Request{Prompt: "p", Provider: "openai"}
	c := ExplicitChain(codex, req)
	if len(c.Legs) != 1 || c.Legs[0].Account != "codex:openai" {
		t.Fatalf("chain = %+v, want one leg on codex:openai", c)
	}
	if _, err := RunWithFallback(context.Background(), c, req, io.Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["codex:openai"]; e.Status(time.Now()) != route.StateExhausted {
		t.Fatalf("codex:openai not marked: %+v", e)
	}
	again := ExplicitChain(codex, req)
	if len(again.Legs) != 0 || len(again.Skipped) != 1 || !strings.Contains(again.Skipped[0], "codex:openai exhausted") {
		t.Fatalf("second explicit chain = %+v, want a gate skip", again)
	}
	if len(codex.seen) != 1 {
		t.Errorf("attempts = %d, want 1 (the gate stops the second)", len(codex.seen))
	}
}

// --native opencode leads on opencode's own config, ahead of (and instead of) the hub cell.
func TestRouteChainNativeOpencodeLeads(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	all := route.Opts{Configured: func(string, route.Cell) error { return nil }}
	c, _, err := RouteChain(context.Background(), "opencode", "sonnet", FallbackAll, false, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Legs[0].Runner.(vendorRunner); !ok || !c.Legs[0].unroutedLeg {
		t.Fatalf("first leg = %+v, want the native opencode runner, unrouted", c.Legs[0])
	}
	for _, l := range c.Legs[1:] {
		if l.Runner.Name() == "opencode" {
			t.Errorf("the hub opencode cell must not also run: %+v", c.Legs)
		}
	}
	none, _, _ := RouteChain(context.Background(), "opencode", "sonnet", FallbackNone, false, true, all)
	if len(none.Legs) != 1 {
		t.Errorf("fallback none = %d legs, want the native leg only", len(none.Legs))
	}
}
