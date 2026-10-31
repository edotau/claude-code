package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
