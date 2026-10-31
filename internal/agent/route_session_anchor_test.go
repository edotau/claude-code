package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestSessionProfileAgents(t *testing.T) {
	const profile, githubProfile = "profile-r7", "profile-k3"
	const gpt, claude = "gpt-6-1-sol", "claude-sonnet-5-5"
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", profile)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+
		`"databricks":{"`+profile+`":{"preset":"databricks","host":"https://profile.example.com",`+
		`"auth":{"type":"pat"},"models":{"sonnet":"`+claude+`"}}},`+
		`"github":{"`+githubProfile+`":{"protocol":"openai","base_url":"https://api.githubcopilot.com",`+
		`"auth":{"type":"cli","login":"copilot"},"models":{"sonnet":"auto"}}}}}`)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: gpt, Label: "GPT", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers[profile]
	if p == nil {
		t.Fatal("profile absent")
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), "profile-auth")
	vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, githubProfile)
	tb, err := LoadFrom(reg, githubProfile)
	if err != nil {
		t.Fatal(err)
	}
	dbx, github := router.ProviderAccount(profile), router.ProviderAccount(githubProfile)
	st := router.State{github: {UsedPct: 100, Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}}
	opts := PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }, WorkRepo: func() bool { return true }}
	pickAgent := func(agent string, state router.State, opts PickOpts) PickResult {
		return tb.Pick(context.Background(), "sonnet", agent, state, opts)
	}
	models := func(r PickResult, agent, account string) []Candidate {
		var out []Candidate
		for _, c := range r.Candidates {
			if c.Agent == agent && c.Account == account {
				out = append(out, c)
			}
		}
		return out
	}
	assertProfile := func(t *testing.T, c Candidate, agent, model string) {
		t.Helper()
		if c.Agent != agent || c.Account != dbx || c.Profile != profile || c.Model != model {
			t.Errorf("agent = %+v, want %s %s profile %s model %s", c, agent, dbx, profile, model)
		}
	}
	t.Run("copilot", func(t *testing.T) {
		r := pickAgent("copilot", st, opts)
		if len(r.Candidates) == 0 {
			t.Fatalf("copilot absent: %+v", r)
		}
		assertProfile(t, r.Candidates[0], "copilot", p.WireModel(p.Models["sonnet"]))
		if len(models(r, "copilot", github)) != 0 || !slices.ContainsFunc(r.Skipped, func(s Skip) bool {
			return s.Agent == "copilot" && s.Gate == GateExhausted
		}) {
			t.Errorf("copilot github profile = %+v", r)
		}
	})
	t.Run("codex", func(t *testing.T) {
		r := pickAgent("codex", st, opts)
		if len(r.Candidates) < 3 {
			t.Fatalf("codex/copilot absent: %+v", r)
		}
		own, _ := tb.Cell("codex", "sonnet")
		if c := r.Candidates[0]; c.Agent != "codex" || c.Account != own.Account || c.Model != own.Model || c.Profile != "" {
			t.Errorf("codex = %+v, want %+v", c, own)
		}
		assertProfile(t, r.Candidates[1], "codex", providers.FirstResponsesModel(p))
		assertProfile(t, r.Candidates[2], "copilot", r.Candidates[1].Model)
		if n := len(models(r, "copilot", dbx)); n != 1 {
			t.Errorf("copilot rows on the session account = %d, want 1: %+v", n, r.Candidates)
		}
		seen := map[string]bool{}
		for _, c := range models(r, "copilot", dbx) {
			if seen[c.Model] {
				t.Errorf("copilot profile model repeated: %+v", c)
			}
			seen[c.Model] = true
		}
	})
	t.Run("copilot auth", func(t *testing.T) {
		blocked := opts
		blocked.Configured = func(agent string, c Cell) error {
			if agent == "copilot" && c.Account == dbx {
				return errors.New("profile auth")
			}
			return nil
		}
		for _, agent := range []string{"copilot", "codex"} {
			r := pickAgent(agent, st, blocked)
			if len(models(r, "copilot", dbx)) != 0 || !slices.ContainsFunc(r.Skipped, func(s Skip) bool {
				return agentOf(s.Agent) == "copilot" && s.Gate == GateAvailable
			}) {
				t.Errorf("%s copilot profile auth = %+v", agent, r)
			}
		}
	})
	t.Run("copilot off", func(t *testing.T) {
		t.Cleanup(func() { vaulttest.Put(t, vault.Pins, providers.PinCopilotActive, "") })
		vaulttest.Put(t, vault.Pins, providers.PinCopilotActive, AgentOff)
		if r := pickAgent("codex", st, opts); slices.ContainsFunc(r.Candidates, func(c Candidate) bool { return c.Agent == "copilot" }) {
			t.Errorf("copilot off = %+v", r)
		}
	})
	t.Run("profile auth", func(t *testing.T) {
		for _, state := range []struct {
			gate  string
			entry router.Entry
		}{
			{router.GateAuth, router.Entry{AuthAt: now0, AuthReason: "profile auth"}},
			{GateExhausted, router.Entry{Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}},
		} {
			blocked := router.State{github: st[github], dbx: state.entry}
			for _, agent := range []string{"copilot", "codex"} {
				r := pickAgent(agent, blocked, opts)
				if len(models(r, "copilot", dbx)) != 0 || !slices.ContainsFunc(r.Skipped, func(s Skip) bool {
					return agentOf(s.Agent) == "copilot" && s.Gate == state.gate
				}) {
					t.Errorf("%s copilot profile %s = %+v", agent, state.gate, r)
				}
			}
		}
	})
	t.Run("copilot profile", func(t *testing.T) {
		t.Cleanup(func() { vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, githubProfile) })
		vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, profile)
		only := opts
		only.Fallback = FallbackNone
		r := pickAgent("copilot", st, only)
		if len(r.Candidates) != 1 {
			t.Fatalf("copilot profile = %+v", r)
		}
		assertProfile(t, r.Candidates[0], "copilot", p.WireModel(p.Models["sonnet"]))
	})
	t.Run("copilot model", func(t *testing.T) {
		own := tb.Rows["copilot"]["sonnet"]
		t.Cleanup(func() {
			tb.Rows["copilot"]["sonnet"] = own
			vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, githubProfile)
		})
		vaulttest.Put(t, vault.Pins, providers.PinCopilotProfile, "")
		cell := own
		cell.Account, cell.Model = dbx, p.WireModel(p.Models["sonnet"])
		tb.Rows["copilot"]["sonnet"] = cell
		only := opts
		only.Fallback = FallbackNone
		if r := pickAgent("copilot", st, only); len(r.Candidates) != 1 || r.Candidates[0].Cell != cell {
			t.Errorf("copilot profile model = %+v, want %+v", r, cell)
		}
	})
	t.Run("codex hop model", func(t *testing.T) {
		own, _ := tb.Cell("codex", "sonnet")
		own.Effort = "high"
		saved := tb.Rows["codex"]["sonnet"]
		t.Cleanup(func() {
			tb.Rows["codex"]["sonnet"] = saved
			if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: gpt, Label: "GPT", Responses: true}}); err != nil {
				t.Fatal(err)
			}
		})
		tb.Rows["codex"]["sonnet"] = own
		menu := []providers.OpencodeModel{{ID: gpt, Label: "GPT", Responses: true}, {ID: own.Model, Label: "Own", Responses: true}}
		if err := providers.SetOpencodeMenu(menu); err != nil {
			t.Fatal(err)
		}
		r := pickAgent("codex", st, opts)
		hops := 0
		for _, c := range r.Candidates {
			if c.Account != dbx || c.Agent == "claude" {
				continue
			}
			hops++
			if c.Model != p.WireModel(own.Model) || c.Effort != "high" {
				t.Errorf("hop = %+v, want model %s effort high", c, p.WireModel(own.Model))
			}
		}
		if hops != 2 {
			t.Errorf("hops = %d, want 2: %+v", hops, r.Candidates)
		}
	})
}
