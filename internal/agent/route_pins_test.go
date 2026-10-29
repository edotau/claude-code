package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type pinFallbackRunner struct {
	name string
	res  Result
	err  error
	seen []Request
}

func (f *pinFallbackRunner) Name() string     { return f.name }
func (f *pinFallbackRunner) Caps() Caps       { return Caps{Model: Native, Effort: Native} }
func (f *pinFallbackRunner) Available() error { return nil }
func (f *pinFallbackRunner) Run(_ context.Context, req Request, _ Sink) (Result, error) {
	f.seen = append(f.seen, req)
	return f.res, f.err
}

func TestPinFirstFallback(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, exhausted bool) (Chain, router.Result, map[string]*pinFallbackRunner) {
		t.Helper()
		vaulttest.UseKeyring(t)
		t.Setenv("HOME", t.TempDir())
		if err := providers.SetPins(map[string]string{
			providers.PinProfile:         "max",
			providers.PinCodexProfile:    "chatgpt",
			providers.PinOpencodeProfile: "opencode-go",
			providers.PinCopilotActive:   providers.AgentOff,
			providers.PinGeminiActive:    providers.AgentOff,
			providers.PinCodexModel:      "gpt-6.1-sol",
		}); err != nil {
			t.Fatal(err)
		}
		if exhausted {
			now := time.Now()
			if err := router.MarkExhausted("anthropic·max", now.Add(time.Hour), now, "test quota"); err != nil {
				t.Fatal(err)
			}
		}
		c, pick, err := RouteChainFor(ctx, "codex", "", "sonnet", FallbackAll, false,
			router.Opts{Configured: func(string, router.Cell) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		fakes := make(map[string]*pinFallbackRunner)
		for i := range c.Legs {
			f := &pinFallbackRunner{name: c.Legs[i].Agent.Name(), res: Result{Answer: "answer from " + c.Legs[i].Agent.Name()}}
			if fakes[f.name] != nil {
				t.Fatalf("duplicate leg %q: %+v", f.name, c.Legs)
			}
			fakes[f.name], c.Legs[i].Agent = f, f
		}
		for _, name := range []string{"codex", "opencode"} {
			if fakes[name] == nil {
				t.Fatalf("missing %s leg: %+v; skipped %v", name, c.Legs, c.Skipped)
			}
		}
		return c, pick, fakes
	}
	failVendors := func(fakes map[string]*pinFallbackRunner) {
		fakes["codex"].res = Result{}
		fakes["codex"].err = &Error{Agent: "codex", Kind: KindQuota, Status: 402, Err: errors.New("402 monthly quota exhausted")}
		fakes["opencode"].res = Result{}
		fakes["opencode"].err = &Error{Agent: "opencode", Kind: KindAuth, Status: 401,
			AuthReason: "refresh token rejected", Err: errors.New("401 refresh token rejected")}
	}
	contains := func(t *testing.T, got string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%q does not contain %q", got, want)
			}
		}
	}

	t.Run("legs", func(t *testing.T) {
		c, pick, _ := setup(t, false)
		var got []string
		for _, leg := range c.Legs {
			got = append(got, leg.Agent.Name()+" ("+leg.Account+")")
		}
		want := []string{"codex (openai·chatgpt)", "opencode (opencode·go)", "claude (anthropic·max)"}
		if !slices.Equal(got, want) {
			t.Fatalf("legs = %v, want %v", got, want)
		}
		if c.Legs[0].Model != "chatgpt:gpt-6.1-sol" || c.Legs[2].Model != "sonnet" {
			t.Errorf("models = %q, %q; want pinned codex and claude at sonnet", c.Legs[0].Model, c.Legs[2].Model)
		}
		for _, name := range []string{"copilot", "gemini"} {
			if !slices.ContainsFunc(pick.Skipped, func(s router.Skip) bool { return s.Agent == name && s.Gate == router.GateOff }) {
				t.Errorf("%s missing off gate: %+v", name, pick.Skipped)
			}
			contains(t, strings.Join(c.Skipped, "; "), name+": deactivated ("+strings.ToUpper(name)+"_ACTIVE=off)")
		}
	})

	t.Run("quota_auth_fallback", func(t *testing.T) {
		c, _, fakes := setup(t, false)
		failVendors(fakes)
		res, err := RunWithFallback(ctx, c, Request{Prompt: "reply"}, Discard, nil)
		if err != nil || res.Agent != "claude" || res.Answer != "answer from claude" || res.Fallback != "codex" {
			t.Fatalf("result = %+v, err = %v", res, err)
		}
		for _, name := range []string{"codex", "opencode", "claude"} {
			if f := fakes[name]; f == nil || len(f.seen) != 1 {
				t.Errorf("%s runner = %+v, want one request", name, f)
			}
		}
		if got := fakes["claude"].seen[0].Model; got != "sonnet" {
			t.Errorf("claude model = %q, want sonnet", got)
		}
		state := router.LoadState()
		if e := state["openai·chatgpt"]; e.Status(time.Now()) != router.StateExhausted {
			t.Errorf("quota.json did not mark openai·chatgpt exhausted: %+v", e)
		}
		if e := state["opencode·go"]; e.AuthReason != "refresh token rejected" || e.AuthAt.IsZero() {
			t.Errorf("quota.json did not mark opencode·go auth: %+v", e)
		}
		contains(t, res.FallbackReason, "codex (openai·chatgpt):", "opencode (opencode·go):")
	})

	t.Run("claude_exhausted", func(t *testing.T) {
		c, _, fakes := setup(t, true)
		if fakes["claude"] != nil {
			t.Fatal("exhausted claude must be skipped")
		}
		failVendors(fakes)
		_, err := RunWithFallback(ctx, c, Request{Prompt: "reply"}, Discard, nil)
		if err == nil {
			t.Fatal("all hops failed, want an error")
		}
		contains(t, err.Error(), "every hop failed", "codex (openai·chatgpt):", "opencode: auth rejected", "claude: anthropic·max exhausted")
	})

	t.Run("content_policy", func(t *testing.T) {
		c, _, fakes := setup(t, false)
		// A policy refusal must stop even when the auth branch would otherwise advance.
		fault := &Error{Agent: "codex", Kind: KindAuth, Status: 403,
			AuthReason: "request rejected", Err: errors.New("403 content_policy refusal")}
		fakes["codex"].res, fakes["codex"].err = Result{}, fault
		_, err := RunWithFallback(ctx, c, Request{Prompt: "reply"}, Discard, nil)
		if !errors.Is(err, fault) {
			t.Errorf("err = %v, want original content-policy fault", err)
		}
		if len(fakes["codex"].seen) != 1 {
			t.Errorf("codex requests = %d, want 1", len(fakes["codex"].seen))
		}
		for _, name := range []string{"opencode", "claude"} {
			if f := fakes[name]; f == nil || len(f.seen) != 0 {
				t.Errorf("%s runner = %+v, want zero requests", name, f)
			}
		}
	})
}
