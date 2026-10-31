package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// ghRow is a github (Copilot) profile row body without its login field.
const ghRow = `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"}`

// ghProfiles is the overlay's "github" entry: one profile per login, named after it, whose login field is it.
func ghProfiles(logins ...string) string {
	rows := make([]string, len(logins))
	for i, l := range logins {
		rows[i] = `"` + l + `":{` + ghRow + `,"login":"` + l + `"}`
	}
	return `"github":{` + strings.Join(rows, ",") + `}`
}

// pinGitHubProfile overlays a github profile for login (plus extra provider entries) and pins it as COPILOT_PROFILE.
func pinGitHubProfile(t *testing.T, login, extra string) {
	t.Helper()
	if extra != "" {
		extra = "," + extra
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+ghProfiles(login)+extra+`}}`)
	setPin(t, providers.PinCopilotProfile, login)
}

// ghLogin makes user a gh login (scratch hosts.yml) pinned as the COPILOT_PROFILE's login, with pct% of credits used.
func ghLogin(t *testing.T, user string, pct float64) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir() // a fake gh answers `gh auth token`, so the pinned login yields a token
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho gho_faketokenfaketokenfaketoken\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cfg := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", cfg)
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
	}
	hosts := "github.com:\n    user: " + user + "\n    users:\n        " + user + ":\n            oauth_token: tok\n"
	if err := os.WriteFile(filepath.Join(cfg, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	pinGitHubProfile(t, user, "")
	prev := CopilotUsedPct
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, true }
	t.Cleanup(func() { CopilotUsedPct = prev })
}

// The GitHub-login provider bills the pinned account's route state; no pin yields no account.
func TestExplicitAccountCopilotLoginIsPinnedUser(t *testing.T) {
	ghLogin(t, "alice", 10)
	acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, RunOptions{Provider: "personal"})
	if acct != "github·alice" {
		t.Errorf("account = %q, want github·alice", acct)
	}
	setPin(t, providers.PinCopilotProfile, "")
	if acct, _ := explicitAccount(&fakeRunner{name: "copilot"}, RunOptions{Provider: "personal"}); acct != "" {
		t.Errorf("no pin: account = %q, want empty", acct)
	}
}

// An exhausted mark on copilot:<pin> does not pre-skip the explicit leg (the gate falls to Databricks), and a
// quota failure on it is recorded on that account.
func TestExplicitChainCopilotNotPreSkippedAndMarks(t *testing.T) {
	ghLogin(t, "alice", 10)
	quota := errors.New("402 Payment Required: You have exceeded your monthly quota")
	cp := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
	req := RunOptions{Prompt: "p", Provider: "personal"}
	c := ExplicitChain(cp, req)
	if len(c.Legs) != 1 || c.Legs[0].Account != "github·alice" {
		t.Fatalf("chain = %+v, want one leg on github·alice", c)
	}
	if _, err := RunWithFallback(context.Background(), c, req, Discard, nil); err == nil {
		t.Fatal("the quota failure must surface")
	}
	if e := loadState()["github·alice"]; e.Status(time.Now()) != router.StateExhausted {
		t.Fatalf("github·alice not marked: %+v", e)
	}
	again := ExplicitChain(cp, req)
	if len(again.Legs) != 1 || len(again.Skipped) != 0 {
		t.Errorf("exhausted GitHub-login leg must stay in the chain: %+v", again)
	}
}

// vendorTargetNote gates an explicit --provider personal (the GitHub login): kept under 95%, refused to Databricks (or an
// error naming no credit-free route) at/over it — never silently another GitHub leg.
func TestVendorTargetNoteExplicitCopilotGated(t *testing.T) {
	req := RunOptions{Prompt: "p", Provider: "personal"}

	ghLogin(t, "alice", 10)
	tg, own, _, err := schemaFor("copilot").targetNote(req)
	if err != nil || own || tg.Provider == nil || tg.Provider.Name != "personal" {
		t.Fatalf("below threshold: %v own=%v err=%v, want personal", tg.Provider, own, err)
	}

	for _, pct := range []float64{95, 99} {
		CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, true }
		tg, _, _, err := schemaFor("copilot").targetNote(req)
		if err == nil && tg.Provider != nil && tg.Provider.Name == "personal" {
			t.Fatalf("pct %v: explicit GitHub login must not be used past the gate", pct)
		}
		if err != nil && !strings.Contains(err.Error(), "no credit-free route") {
			t.Errorf("pct %v: err = %v, want the no credit-free route reason", pct, err)
		}
	}
}

// fakeCopilotBin installs an executable stand-in at BinDir()/copilot so paths.VendorBin resolves.
func fakeCopilotBin(t *testing.T) {
	t.Helper()
	dir := paths.BinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copilot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stubAsk swaps copilotAsk for fn (restored at cleanup) and returns the recorded call count.
func stubAsk(t *testing.T, fn func(n int) (copilotsdk.Result, error)) *int {
	t.Helper()
	prev, n := copilotAsk, 0
	copilotAsk = func(context.Context, copilotsdk.Options, string, []string) (copilotsdk.Result, error) {
		n++
		return fn(n)
	}
	t.Cleanup(func() { copilotAsk = prev })
	return &n
}

// An exit-0 empty answer from the SDK is a KindFatal emptyAnswer("copilot"), not a success.
func TestCopilotRunnerEmptyAnswerIsFatal(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) { return copilotsdk.Result{Text: "  \n", SessionID: "s1"}, nil })
	res, err := copilotAgent.Run(context.Background(), RunOptions{Prompt: "p", Provider: "personal", Direct: true}, Discard)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindFatal || e.Agent != "copilot" || !strings.Contains(e.Err.Error(), "empty answer") {
		t.Fatalf("err = %v, want KindFatal emptyAnswer(copilot)", err)
	}
	if res.SessionID != "s1" {
		t.Errorf("session id must survive the empty answer, got %q", res.SessionID)
	}
}

// A 402 marks the pinned GitHub account and writes a hand-off without rerunning the SDK.
func TestCopilotRunnerQuotaTriggersHandoff(t *testing.T) {
	dir, marked := stubHandoff(t)
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	calls := stubAsk(t, func(n int) (copilotsdk.Result, error) {
		if n != 1 {
			t.Fatal("quota handoff must not rerun the SDK")
		}
		return copilotsdk.Result{Text: "Step 1 done.", SessionID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}, errQuotaMidTask
	})
	res, err := copilotAgent.Run(context.Background(), RunOptions{Prompt: "Refactor foo", Model: "opus", Provider: "personal", Direct: true}, Discard)
	if !errors.Is(err, errQuotaMidTask) || Classify("copilot", err, false).Kind != KindQuota {
		t.Fatalf("res=%+v err=%v; want the KindQuota cause", res, err)
	}
	if *calls != 1 {
		t.Errorf("SDK calls = %d, want 1", *calls)
	}
	if len(*marked) != 1 || (*marked)[0] != "github·alice" {
		t.Errorf("marked = %v, want github·alice", *marked)
	}
	if filepath.Dir(res.Handoff) != dir {
		t.Fatalf("handoff = %q, want a file under %s", res.Handoff, dir)
	}
	if _, err := os.Stat(res.Handoff); err != nil {
		t.Fatal(err)
	}
}

// setPin seeds (value != "") or clears a vault pin; pins are vault-only, never read from env.
func setPin(t *testing.T, key, value string) {
	t.Helper()
	if err := providers.SetPins(map[string]string{key: value}); err != nil {
		t.Fatal(err)
	}
}

// Partial copilot text followed by a transient error is not retried: one SDK call, the partial answer and session kept.
func TestCopilotPartialAnswerKeptOnTransientError(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	calls := stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "half an answer", SessionID: "sess-9"}, errors.New("send prompt: connection reset")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := Run(ctx, copilotAgent, RunOptions{Prompt: "p", Provider: "personal", Direct: true}, Discard, nil)
	if err == nil {
		t.Fatal("err = nil, want the SDK failure")
	}
	if *calls != 1 {
		t.Errorf("copilotAsk calls = %d, want 1: output was already produced, a retry would re-run the task", *calls)
	}
	if res.Handoff != "" {
		t.Fatalf("Handoff = %q: this must be the NON-quota path", res.Handoff)
	}
	if res.Answer != "half an answer" {
		t.Errorf("Answer = %q, want the partial text \"half an answer\"", res.Answer)
	}
	if res.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want \"sess-9\"", res.SessionID)
	}
}

// Copilot reports no token usage, so a successful run's envelope has usage null and fallback null.
func TestCopilotResultJSONUsageNull(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "hello", SessionID: "sess-1", Model: "m"}, nil
	})
	res, err := Run(context.Background(), copilotAgent, RunOptions{Prompt: "p", Provider: "personal", Direct: true}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(NewEnvelope(res, nil, "id-1"))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["usage"]; !ok || v != nil {
		t.Errorf("usage = %v (present=%v), want explicit null: copilot reports none", v, ok)
	}
	if v, ok := m["fallback"]; !ok || v != nil {
		t.Errorf("fallback = %v, want null when the requested agent answered", m["fallback"])
	}
}

// gateReg is the real registry plus keyed Databricks rows dev/qa/prod; env names which ones mint a credential.
func gateReg(t *testing.T, ok map[string]bool) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "qa", "prod"} {
		key := "GATE_TEST_DBX_" + strings.ToUpper(env)
		t.Setenv(key, "")
		if ok[env] {
			t.Setenv(key, "tok-"+env)
		}
		reg.Providers[env] = &providers.Provider{
			Name: env, Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks, ModelPrefix: "databricks-",
			Models: map[string]string{"opus": "databricks-claude-opus-5-5"},
			Auth:   providers.Auth{Type: providers.AuthKey, Env: key},
			Routes: map[string]string{providers.ProtocolOpenAI: "https://h.example.com/serving-endpoints"},
		}
	}
	return reg
}

// gateSetup pins user at pct used (ok=false = lookup failed) and returns the github-copilot login target.
func gateSetup(t *testing.T, pct float64, ok bool, reg *providers.Registry) providers.Target {
	t.Helper()
	isolateOpencodeRouteEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	workableCopilotPin(t, "alice")
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return pct, ok }
	gh := reg.Providers["personal"]
	if gh == nil || gh.Auth.Login != "copilot" {
		t.Fatalf("github-copilot login provider missing: %+v", gh)
	}
	return providers.Target{Provider: gh, Model: gh.Models["opus"]}
}

// TestCopilotGateThreshold pins the 95% line: below keeps GitHub, at/over/unverified falls back to Databricks.
func TestCopilotGateThreshold(t *testing.T) {
	tests := []struct {
		name       string
		pct        float64
		ok         bool
		wantGitHub bool
	}{
		{"below", 94.9, true, true},
		{"at 95", 95, true, false},
		{"over", 99.5, true, false},
		{"exhausted 100", 100, true, false},
		{"lookup failed", 10, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := gateReg(t, map[string]bool{"dev": true})
			gh := gateSetup(t, tt.pct, tt.ok, reg)
			if _, got := copilotHeadroom(context.Background(), "alice"); got != tt.wantGitHub {
				t.Errorf("copilotHeadroom ok = %v, want %v", got, tt.wantGitHub)
			}
			got, note, err := copilotGate(reg, gh)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantGitHub {
				if got.Provider != gh.Provider || note != "" {
					t.Errorf("workable pin must keep GitHub silently: %v %q", got.Provider.Name, note)
				}
				if ri := describeCopilotRoute(reg); ri.Leg != LegCopilotLogin {
					t.Errorf("describe leg = %q, want copilot-login", ri.Leg)
				}
				return
			}
			if got.Provider.Name != "dev" || !strings.Contains(note, "alice") {
				t.Errorf("want dev with a notice naming alice, got %s %q", got.Provider.Name, note)
			}
			if ri := describeCopilotRoute(reg); ri.Leg != LegDatabricks || ri.Provider != "dev" {
				t.Errorf("describe = %+v, want databricks dev", ri)
			}
		})
	}
}

// TestCopilotGateUnsetLookupAndPin pins fallback when the usage lookup is unset, there is no pin, or the pin is not a gh login.
func TestCopilotGateUnsetLookupAndPin(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"lookup unset": func(t *testing.T) { CopilotUsedPct = nil },
		"no pin":       func(t *testing.T) { setPin(t, providers.PinCopilotProfile, "") },
		"pin not a gh login": func(t *testing.T) {
			setPin(t, providers.PinCopilotProfile, "mallory")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			reg := gateReg(t, map[string]bool{"qa": true})
			gh := gateSetup(t, 1, true, reg)
			mutate(t)
			if _, ok := copilotHeadroom(context.Background(), providers.Pin(providers.PinCopilotProfile)); ok {
				t.Fatal("must not be workable")
			}
			got, _, err := copilotGate(reg, gh)
			if err != nil || got.Provider.Name != "qa" {
				t.Errorf("gate = %v %v, want qa fallback", got.Provider, err)
			}
		})
	}
}

// TestResolveTargetExplicitCopilotGated pins that --provider github-copilot (explicit) obeys the same 95% rule.
func TestResolveTargetExplicitCopilotGated(t *testing.T) {
	for _, tt := range []struct {
		pct      float64
		wantName string
	}{{10, "personal"}, {95, "dev"}, {99, "dev"}} {
		t.Run(strconv.FormatFloat(tt.pct, 'f', -1, 64), func(t *testing.T) {
			reg := gateReg(t, map[string]bool{"dev": true})
			gh := gateSetup(t, tt.pct, true, reg)
			sel := providers.Selection{Provider: gh.Provider, Slots: map[string]providers.Target{"opus": gh}}
			got, _, err := schemaFor("copilot").ResolveTarget(reg, sel, true)
			if err != nil || got.Provider.Name != tt.wantName {
				t.Errorf("explicit resolve = %v %v, want %s", got.Provider, err, tt.wantName)
			}
			// CanServe path (not explicit) is gated the same way.
			got, _, err = schemaFor("copilot").ResolveTarget(reg, sel, false)
			if err != nil || got.Provider.Name != tt.wantName {
				t.Errorf("CanServe resolve = %v %v, want %s", got.Provider, err, tt.wantName)
			}
		})
	}
}

// TestCopilotGateNonLoginProviderUntouched pins that a non-GitHub target passes the gate unchanged.
func TestCopilotGateNonLoginProviderUntouched(t *testing.T) {
	reg := gateReg(t, map[string]bool{"dev": true})
	gateSetup(t, 99, true, reg)
	in := providers.Target{Provider: reg.Providers["qa"], Model: "m"}
	got, note, err := copilotGate(reg, in)
	if err != nil || got != in || note != "" {
		t.Errorf("gate altered a non-login target: %+v %q %v", got, note, err)
	}
}

// TestCopilotDatabricksFallbackOrder pins vault pin databricks row, then first OK env, then LegNone — never GitHub.
func TestCopilotDatabricksFallbackOrder(t *testing.T) {
	t.Run("vault pin databricks row wins", func(t *testing.T) {
		reg := gateReg(t, map[string]bool{"dev": true, "prod": true})
		isolateOpencodeRouteEnv(t)
		if err := providers.SetPins(map[string]string{providers.PinProfile: "prod"}); err != nil {
			t.Fatal(err)
		}
		if ri := copilotDatabricksFallback(reg, "d"); !ri.OK || ri.Provider != "prod" || ri.Leg != LegDatabricks {
			t.Errorf("got %+v, want prod", ri)
		}
	})
	t.Run("non-databricks pin ignored, first OK env", func(t *testing.T) {
		reg := gateReg(t, map[string]bool{"qa": true}) // dev and prod fail
		isolateOpencodeRouteEnv(t)
		if err := providers.SetPins(map[string]string{providers.PinProfile: "anthropic"}); err != nil {
			t.Fatal(err)
		}
		if ri := copilotDatabricksFallback(reg, "d"); !ri.OK || ri.Provider != "qa" {
			t.Errorf("got %+v, want qa (the only OK row)", ri)
		}
	})
	t.Run("all failing is LegNone, never GitHub", func(t *testing.T) {
		reg := gateReg(t, nil)
		gh := gateSetup(t, 99, true, reg)
		ri := copilotDatabricksFallback(reg, "alice 99.0% of credits used")
		if ri.OK || ri.Leg != LegNone || !strings.Contains(ri.Reason, "no credit-free route for copilot") {
			t.Errorf("got %+v", ri)
		}
		if _, _, err := copilotGate(reg, gh); err == nil || !strings.Contains(err.Error(), "no credit-free route") {
			t.Errorf("gate err = %v, want no credit-free route", err)
		}
		if d := describeCopilotRoute(reg); d.Leg == LegCopilotLogin || d.Provider == "personal" {
			t.Errorf("describe must never fall to GitHub: %+v", d)
		}
	})
}

// TestCopilotPromptCap pins the budget: window - 32k - 10% (1M -> 868000, 200k -> 148000).
func TestCopilotPromptCap(t *testing.T) {
	for _, tt := range []struct {
		name  string
		model string
		oneM  bool
		want  string
	}{
		{"target 1M marked", "gpt-5", true, "868000"},
		{"standard 200k", "gpt-5", false, "148000"},
		{"endpoint not serving 1M, claude model", "claude-opus-5-5", false, "148000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tg := target(t, "openai", tt.model)
			tg.OneM = tt.oneM
			env, _, err := schemaFor("copilot").Configure(WithDirect(context.Background()), tg, "")
			if err != nil {
				t.Fatal(err)
			}
			if got := envMap(env)["COPILOT_PROVIDER_MAX_PROMPT_TOKENS"]; got != tt.want {
				t.Errorf("MAX_PROMPT_TOKENS = %q, want %s", got, tt.want)
			}
		})
	}
}

// (6) A vault-pinned Databricks row whose credential fails is skipped for the next OK row; a keyed pin still wins.
func TestCopilotFallbackSkipsPinnedRowWithFailingCredential(t *testing.T) {
	custom := func(reg *providers.Registry, env string) {
		reg.Providers["workspace-x"] = &providers.Provider{
			Name: "workspace-x", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks, ModelPrefix: "databricks-",
			Models: map[string]string{"opus": "databricks-claude-opus-5-5"},
			Auth:   providers.Auth{Type: providers.AuthKey, Env: env},
			Routes: map[string]string{providers.ProtocolOpenAI: "https://h.example.com/serving-endpoints"},
		}
	}
	t.Run("failing pin skipped", func(t *testing.T) {
		reg := gateReg(t, map[string]bool{"qa": true})
		isolateOpencodeRouteEnv(t)
		t.Setenv("GATE_TEST_WSX", "")
		custom(reg, "GATE_TEST_WSX")
		if err := providers.SetPins(map[string]string{providers.PinProfile: "workspace-x"}); err != nil {
			t.Fatal(err)
		}
		if ri := copilotDatabricksFallback(reg, "d"); !ri.OK || ri.Provider != "qa" {
			t.Errorf("got %+v, want qa after the pinned row's credential failed", ri)
		}
	})
	t.Run("keyed pin wins", func(t *testing.T) {
		reg := gateReg(t, map[string]bool{"qa": true})
		isolateOpencodeRouteEnv(t)
		t.Setenv("GATE_TEST_WSX", "tok")
		custom(reg, "GATE_TEST_WSX")
		if err := providers.SetPins(map[string]string{providers.PinProfile: "workspace-x"}); err != nil {
			t.Fatal(err)
		}
		if ri := copilotDatabricksFallback(reg, "d"); !ri.OK || ri.Provider != "workspace-x" {
			t.Errorf("got %+v, want workspace-x", ri)
		}
	})
}
