package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// pinFirstSetup configures codex pinned to openai with opencode and claude behind it, each swapped for a stub.
func pinFirstSetup(t *testing.T, exhausted bool) (Chain, router.PickResult, map[string]*pinFallbackRunner) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key") // the non-own-login openai profile needs a credential
	vaulttest.SetRecord(t, "claude", "profile", "ultra")
	vaulttest.SetRecord(t, "codex", "profile", "openai")
	vaulttest.SetRecord(t, "opencode", "profile", "opencode-go")
	vaulttest.SetRecord(t, "copilot", "profile", "")
	vaulttest.SetRecord(t, "gemini", "profile", "")
	vaulttest.SetRecord(t, "codex", "model", "gpt-6.1-sol")
	if exhausted {
		now := time.Now()
		if err := router.MarkExhausted("anthropic·ultra", now.Add(time.Hour), now, "test quota"); err != nil {
			t.Fatal(err)
		}
	}
	c, pick, err := RouteChainFor(context.Background(), "codex", "", "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured})
	if err != nil {
		t.Fatal(err)
	}
	fakes := make(map[string]*pinFallbackRunner)
	for i := range c.Dispatches {
		f := &pinFallbackRunner{name: c.Dispatches[i].Agent.Name(), res: wire.Result{Answer: "answer from " + c.Dispatches[i].Agent.Name()}}
		if fakes[f.name] != nil {
			t.Fatalf("duplicate dispatch %q: %+v", f.name, c.Dispatches)
		}
		fakes[f.name], c.Dispatches[i].Agent = f, f
	}
	for _, name := range []string{"codex", "opencode"} {
		if fakes[name] == nil {
			t.Fatalf("missing %s dispatch: %+v; skipped %v", name, c.Dispatches, c.Skipped)
		}
	}
	return c, pick, fakes
}

func failPinVendors(fakes map[string]*pinFallbackRunner) {
	fakes["codex"].res = wire.Result{}
	fakes["codex"].err = &retry.Error{Agent: "codex", Kind: retry.KindQuota, Status: 402, Err: errors.New("402 monthly quota exhausted")}
	fakes["opencode"].res = wire.Result{}
	fakes["opencode"].err = &retry.Error{Agent: "opencode", Kind: retry.KindAuth, Status: 401,
		AuthReason: "refresh token rejected", Err: errors.New("401 refresh token rejected")}
}

func assertPinVendorsMarked(t *testing.T) {
	t.Helper()
	state := router.LoadState()
	if e := state["openai·openai"]; e.Status(time.Now()) != router.StateExhausted {
		t.Errorf("quota.json did not mark openai·openai exhausted: %+v", e)
	}
	if e := state["opencode·go"]; e.AuthReason != "refresh token rejected" || e.AuthAt.IsZero() {
		t.Errorf("quota.json did not mark opencode·go auth: %+v", e)
	}
}

func containsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not contain %q", got, want)
		}
	}
}

func TestPinFirstFallbackDispatches(t *testing.T) {
	c, pick, _ := pinFirstSetup(t, false)
	var got []string
	for _, disp := range c.Dispatches {
		got = append(got, disp.Agent.Name()+" ("+disp.Account+")")
	}
	want := []string{"codex (openai·openai)", "opencode (opencode·go)", "claude (anthropic·ultra)"}
	if !slices.Equal(got, want) {
		t.Fatalf("dispatches = %v, want %v", got, want)
	}
	if c.Dispatches[0].Model != "openai:gpt-6.1-sol" || c.Dispatches[2].Model != "sonnet" {
		t.Errorf("models = %q, %q; want pinned codex and claude at sonnet", c.Dispatches[0].Model, c.Dispatches[2].Model)
	}
	for _, name := range []string{"copilot", "gemini"} {
		if !slices.ContainsFunc(pick.Skipped, func(s router.Skip) bool { return s.Agent == name && s.Gate == router.GateOff }) {
			t.Errorf("%s missing off gate: %+v", name, pick.Skipped)
		}
		containsAll(t, strings.Join(c.Skipped, "; "), name+": deactivated ("+strings.ToUpper(name)+"_ACTIVE=off)")
	}
}

func TestPinFirstFallbackQuotaAuthFallback(t *testing.T) {
	c, _, fakes := pinFirstSetup(t, false)
	failPinVendors(fakes)
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "reply"}, wire.Discard, nil)
	if err != nil || res.Agent != "claude" || res.Answer != "answer from claude" || res.Fallback != "codex" {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	for _, name := range []string{"codex", "opencode", "claude"} {
		if f := fakes[name]; f == nil || len(f.seen) != 1 {
			t.Errorf("%s agent = %+v, want one request", name, f)
		}
	}
	if got := fakes["claude"].seen[0].Model; got != "sonnet" {
		t.Errorf("claude model = %q, want sonnet", got)
	}
	assertPinVendorsMarked(t)
	containsAll(t, res.FallbackReason, "codex (openai·openai):", "opencode (opencode·go):")
}

func TestPinFirstFallbackClaudeExhausted(t *testing.T) {
	c, _, fakes := pinFirstSetup(t, true)
	if fakes["claude"] != nil {
		t.Fatal("exhausted claude must be skipped")
	}
	failPinVendors(fakes)
	_, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "reply"}, wire.Discard, nil)
	if err == nil {
		t.Fatal("all hops failed, want an error")
	}
	containsAll(t, err.Error(), "every hop failed", "codex (openai·openai):", "opencode: auth rejected", "claude: anthropic·ultra exhausted")
}

func TestPinFirstFallbackContentPolicy(t *testing.T) {
	c, _, fakes := pinFirstSetup(t, false)
	// A policy refusal must stop even when the auth branch would otherwise advance.
	fault := &retry.Error{Agent: "codex", Kind: retry.KindAuth, Status: 403,
		AuthReason: "request rejected", Err: errors.New("403 content_policy refusal")}
	fakes["codex"].res, fakes["codex"].err = wire.Result{}, fault
	_, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "reply"}, wire.Discard, nil)
	if !errors.Is(err, fault) {
		t.Errorf("err = %v, want original content-policy fault", err)
	}
	if len(fakes["codex"].seen) != 1 {
		t.Errorf("codex requests = %d, want 1", len(fakes["codex"].seen))
	}
	for _, name := range []string{"opencode", "claude"} {
		if f := fakes[name]; f == nil || len(f.seen) != 0 {
			t.Errorf("%s agent = %+v, want zero requests", name, f)
		}
	}
}

const (
	sessProfile       = "profile-r7"
	sessGithubProfile = "profile-k3"
	sessGPT           = "gpt-6-1-sol"
	sessClaude        = "claude-sonnet-5-5"
)

// sessFix is a Databricks session profile plus a github copilot profile, with github already exhausted.
type sessFix struct {
	p           *providers.Provider
	tb          *router.RouteTable
	dbx, github string
	st          router.State
	opts        router.PickOpts
}

func newSessFix(t *testing.T) *sessFix {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", sessProfile)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+
		`"databricks":{"`+sessProfile+`":{"preset":"databricks","host":"https://profile.example.com",`+
		`"auth":{"type":"pat"},"models":{"sonnet":"`+sessClaude+`"}}},`+
		`"github":{"`+sessGithubProfile+`":{"protocol":"openai","base_url":"https://api.githubcopilot.com",`+
		`"auth":{"type":"cli","login":"copilot"},"models":{"sonnet":"auto"}}}}}`)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: sessGPT, Label: "GPT", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers[sessProfile]
	if p == nil {
		t.Fatal("profile absent")
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), "profile-auth")
	vaulttest.SetRecord(t, "copilot", "profile", sessGithubProfile)
	activateAgents(t, "codex", "opencode")
	tb, err := router.LoadFrom(reg, sessGithubProfile)
	if err != nil {
		t.Fatal(err)
	}
	dbx, github := router.ProviderAccount(sessProfile), router.ProviderAccount(sessGithubProfile)
	return &sessFix{p: p, tb: tb, dbx: dbx, github: github,
		st:   router.State{github: {UsedPct: 100, Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}},
		opts: router.PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }, WorkRepo: func() bool { return true }}}
}

func (s *sessFix) pickAgent(t *testing.T, agent string, state router.State, opts router.PickOpts) router.PickResult {
	t.Helper()
	opts, err := RosterPick(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s.tb.Pick(context.Background(), "sonnet", agent, state, opts)
}

func (s *sessFix) models(r router.PickResult, agent, account string) []router.Candidate {
	var out []router.Candidate
	for _, c := range r.Candidates {
		if c.Agent == agent && c.Account == account {
			out = append(out, c)
		}
	}
	return out
}

func (s *sessFix) assertProfile(t *testing.T, c router.Candidate, agent, model string) {
	t.Helper()
	if c.Agent != agent || c.Account != s.dbx || c.Profile != sessProfile || c.Model != model {
		t.Errorf("agent = %+v, want %s %s profile %s model %s", c, agent, s.dbx, sessProfile, model)
	}
}

func (s *sessFix) skipped(r router.PickResult, agent, gate string) bool {
	return slices.ContainsFunc(r.Skipped, func(k router.Skip) bool {
		return strings.Split(k.Agent, "·")[0] == agent && k.Gate == gate
	})
}

func TestSessionProfileAgentsCopilot(t *testing.T) {
	s := newSessFix(t)
	r := s.pickAgent(t, "copilot", s.st, s.opts)
	if len(r.Candidates) == 0 {
		t.Fatalf("copilot absent: %+v", r)
	}
	s.assertProfile(t, r.Candidates[0], "copilot", s.p.WireModel(s.p.Models["sonnet"]))
	if len(s.models(r, "copilot", s.github)) != 0 || !s.skipped(r, "copilot", router.GateExhausted) {
		t.Errorf("copilot github profile = %+v", r)
	}
}

func TestSessionProfileAgentsCodex(t *testing.T) {
	s := newSessFix(t)
	r := s.pickAgent(t, "codex", s.st, s.opts)
	if len(r.Candidates) < 3 {
		t.Fatalf("codex/copilot absent: %+v", r)
	}
	own, _ := s.tb.Cell("codex", "sonnet")
	if c := r.Candidates[0]; c.Agent != "codex" || c.Account != own.Account || c.Model != own.Model || c.Profile != "" {
		t.Errorf("codex = %+v, want %+v", c, own)
	}
	s.assertProfile(t, r.Candidates[1], "codex", providers.FirstResponsesModel(s.p))
	s.assertProfile(t, r.Candidates[2], "copilot", r.Candidates[1].Model)
	if n := len(s.models(r, "copilot", s.dbx)); n != 1 {
		t.Errorf("copilot rows on the session account = %d, want 1: %+v", n, r.Candidates)
	}
	seen := map[string]bool{}
	for _, c := range s.models(r, "copilot", s.dbx) {
		if seen[c.Model] {
			t.Errorf("copilot profile model repeated: %+v", c)
		}
		seen[c.Model] = true
	}
}

func TestSessionProfileAgentsCopilotAuth(t *testing.T) {
	s := newSessFix(t)
	blocked := s.opts
	blocked.Configured = func(agent string, c router.Cell) error {
		if agent == "copilot" && c.Account == s.dbx {
			return errors.New("profile auth")
		}
		return nil
	}
	for _, agent := range []string{"copilot", "codex"} {
		r := s.pickAgent(t, agent, s.st, blocked)
		if len(s.models(r, "copilot", s.dbx)) != 0 || !s.skipped(r, "copilot", router.GateAvailable) {
			t.Errorf("%s copilot profile auth = %+v", agent, r)
		}
	}
}

func TestSessionProfileAgentsCopilotOff(t *testing.T) {
	s := newSessFix(t)
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if r := s.pickAgent(t, "codex", s.st, s.opts); slices.ContainsFunc(r.Candidates, func(c router.Candidate) bool { return c.Agent == "copilot" }) {
		t.Errorf("copilot off = %+v", r)
	}
}

func TestSessionProfileAgentsProfileAuth(t *testing.T) {
	s := newSessFix(t)
	for _, state := range []struct {
		gate  string
		entry router.Entry
	}{
		{router.GateAuth, router.Entry{AuthAt: now0, AuthReason: "profile auth"}},
		{router.GateExhausted, router.Entry{Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}},
	} {
		blocked := router.State{s.github: s.st[s.github], s.dbx: state.entry}
		for _, agent := range []string{"copilot", "codex"} {
			r := s.pickAgent(t, agent, blocked, s.opts)
			if len(s.models(r, "copilot", s.dbx)) != 0 || !s.skipped(r, "copilot", state.gate) {
				t.Errorf("%s copilot profile %s = %+v", agent, state.gate, r)
			}
		}
	}
}

func TestSessionProfileAgentsCopilotProfile(t *testing.T) {
	s := newSessFix(t)
	vaulttest.SetRecord(t, "copilot", "profile", sessProfile)
	only := s.opts
	only.Fallback = router.FallbackNone
	r := s.pickAgent(t, "copilot", s.st, only)
	if len(r.Candidates) != 1 {
		t.Fatalf("copilot profile = %+v", r)
	}
	s.assertProfile(t, r.Candidates[0], "copilot", s.p.WireModel(s.p.Models["sonnet"]))
}

func TestSessionProfileAgentsCopilotModel(t *testing.T) {
	s := newSessFix(t)
	cell := s.tb.Rows["copilot"]["sonnet"]
	cell.Account, cell.Model = s.dbx, s.p.WireModel(s.p.Models["sonnet"])
	s.tb.Rows["copilot"]["sonnet"] = cell
	only := s.opts
	only.Fallback = router.FallbackNone
	if r := s.pickAgent(t, "copilot", s.st, only); len(r.Candidates) != 1 || r.Candidates[0].Cell != cell {
		t.Errorf("copilot profile model = %+v, want %+v", r, cell)
	}
}

func TestSessionProfileAgentsCodexHopModel(t *testing.T) {
	s := newSessFix(t)
	own, _ := s.tb.Cell("codex", "sonnet")
	own.Effort = "high"
	s.tb.Rows["codex"]["sonnet"] = own
	menu := []providers.OpencodeModel{{ID: sessGPT, Label: "GPT", Responses: true}, {ID: own.Model, Label: "Own", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	r := s.pickAgent(t, "codex", s.st, s.opts)
	hops := 0
	for _, c := range r.Candidates {
		if c.Account != s.dbx || c.Agent == "claude" {
			continue
		}
		hops++
		if c.Model != s.p.WireModel(own.Model) || c.Effort != "high" {
			t.Errorf("hop = %+v, want model %s effort high", c, s.p.WireModel(own.Model))
		}
	}
	if hops != 2 {
		t.Errorf("hops = %d, want 2: %+v", hops, r.Candidates)
	}
}
