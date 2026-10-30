package configure

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

var setupAgentNames = []string{"claude", "codex", "gemini", "opencode", "copilot"}

// setupFake swaps every Set up seam; calls records the step order, the errors fail that step.
type setupFake struct {
	calls                      []string
	verified                   bool
	loginErr, verifyErr, rtErr error
	changed                    bool
}

func installSetupFake(t *testing.T, f *setupFake) {
	t.Helper()
	st, ve, ro := setupState, setupVerify, setupRoute
	logins := map[string]func(*dialog) error{}
	for k, v := range setupLogins {
		logins[k] = v
		setupLogins[k] = func(*dialog) error { f.calls = append(f.calls, "login"); return f.loginErr }
	}
	t.Cleanup(func() {
		setupState, setupVerify, setupRoute = st, ve, ro
		for k, v := range logins {
			setupLogins[k] = v
		}
	})
	setupState = func(*dialog, string) agentState { return agentState{verified: f.verified} }
	setupVerify = func(context.Context, string) error { f.calls = append(f.calls, "verify"); return f.verifyErr }
	setupRoute = func(*dialog, string) (bool, error) { f.calls = append(f.calls, "route"); return f.changed, f.rtErr }
}

func setupDialog(t *testing.T) (*dialog, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return &dialog{reg: menuTestEnv(t), out: out}, out
}

func TestAgentSetUpOrderAndStops(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name        string
		f           setupFake
		calls, want string
		notWant     string
	}{
		{"success", setupFake{changed: true}, "login,verify,route", "verified", "stopped"},
		{"already verified skips sign-in", setupFake{verified: true, changed: true}, "verify,route", "already signed in", "stopped"},
		{"sign-in fails", setupFake{loginErr: boom}, "login", "set up stopped at sign-in: boom", "verified"},
		{"verify fails", setupFake{verifyErr: boom}, "login,verify", "set up stopped at verify: boom", "route was not"},
		{"route fails", setupFake{rtErr: boom}, "login,verify,route", "set up stopped at route: boom", "route was not"},
		{"route cancelled", setupFake{}, "login,verify,route", "signed in; the route was not changed", "stopped"},
	} {
		for _, agent := range setupAgentNames {
			t.Run(c.name+"/"+agent, func(t *testing.T) {
				d, out := setupDialog(t)
				f := c.f
				installSetupFake(t, &f)
				agentSetUp(d, agent)
				if got := strings.Join(f.calls, ","); got != c.calls {
					t.Errorf("calls = %s, want %s", got, c.calls)
				}
				prefix := displayLabel(agent) + ": "
				if !strings.Contains(out.String(), prefix) || !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), c.notWant) {
					t.Errorf("out = %q, want %q%q without %q", out.String(), prefix, c.want, c.notWant)
				}
			})
		}
	}
}

func TestAgentSetUpCopilotCreditsReadAsNotLoginFailure(t *testing.T) {
	d, out := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("status quota")})
	agentSetUp(d, "copilot")
	for _, w := range []string{"copilot: set up stopped at verify: signed in, but copilot has no credits left", "not a login failure"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("out = %q, want %q", out.String(), w)
		}
	}
	d2, out2 := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("status quota")})
	agentSetUp(d2, "codex")
	if strings.Contains(out2.String(), "credits") {
		t.Errorf("only copilot gets the credits message: %q", out2.String())
	}
}

func TestAgentSetUpNeverPrintsAToken(t *testing.T) {
	tok := "Bearer " + strings.Repeat("s3cr3tTOKEN", 5)
	d, out := setupDialog(t)
	installSetupFake(t, &setupFake{verifyErr: errors.New("401 with " + tok)})
	agentSetUp(d, "codex")
	if !strings.Contains(out.String(), "set up stopped at verify") || strings.Contains(out.String(), "s3cr3tTOKEN") {
		t.Errorf("out = %q", out.String())
	}
}

func TestSetUpIsEveryAgentPagesFirstRow(t *testing.T) {
	for _, agent := range setupAgentNames {
		d, _ := setupDialog(t)
		f := &setupFake{changed: true}
		installSetupFake(t, f)
		_, _, rows := fakePickAgent(t, 0)
		runAgentMenu(d, displayLabel(agent), agentMenus[agent](d))
		if got := (*rows)[0]; got != [2]string{"Set up", "sign in, verify, pin the route"} {
			t.Errorf("%s first row = %v", agent, got)
		}
		if strings.Join(f.calls, ",") != "login,verify,route" {
			t.Errorf("%s calls = %v", agent, f.calls)
		}
		if !slices.ContainsFunc(agentPinRows(d, agent), func(r billingRow) bool { return r.label == "Route" }) {
			t.Errorf("%s has no Route row for the configure step", agent)
		}
	}
}
