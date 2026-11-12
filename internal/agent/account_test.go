package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex served by an OpenAI API key bills openai·openai (credits), not the ChatGPT plan's key.
func TestResolveAccountNamesActualBackend(t *testing.T) {
	setConfigDir(t)
	vaulttest.SetRecord(t, "claude", "profile", "openai")
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
	setConfigDir(t)
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	t.Setenv(providers.EnvSessionProfile, "openai")
	if acct, _ := resolveAccount("claude", router.Cell{Model: "claude-sonnet-5-5"}); acct != "openai·openai" {
		t.Errorf("claude account = %q, want the session profile openai·openai", acct)
	}
	if acct, _ := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "openai·chatgpt"}); acct != "" {
		t.Errorf("codex login cell account = %q, want it kept (empty override)", acct)
	}
}

// An explicit backend stays one dispatch, records its own quota failure, and is skipped without an attempt after.
func TestExplicitChainRecordsAndGates(t *testing.T) {
	setConfigDir(t)
	codex := &fakeRunner{name: "codex", errs: []error{errors.New("insufficient_quota: you exceeded your current quota")}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "openai"}
	c := explicitChain(codex, req)
	if len(c.Dispatches) != 1 || c.Dispatches[0].Account != "openai·openai" {
		t.Fatalf("chain = %+v, want one dispatch on openai·openai", c)
	}
	if _, err := runWithFallback(context.Background(), c, req, wire.Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["openai·openai"]; e.Status(time.Now()) != router.StateExhausted {
		t.Fatalf("openai·openai not marked: %+v", e)
	}
	again := explicitChain(codex, req)
	if len(again.Dispatches) != 0 || len(again.Skipped) != 1 || !strings.Contains(again.Skipped[0], "openai·openai exhausted") {
		t.Fatalf("second explicit chain = %+v, want a gate skip", again)
	}
	if len(codex.seen) != 1 {
		t.Errorf("attempts = %d, want 1 (the gate stops the second)", len(codex.seen))
	}
}
