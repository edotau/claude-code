package menu

import (
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/vault"
)

// setupLogins is each agent's existing sign-in path; an agent absent here has no Set up row.
var setupLogins = map[string]func(d *dialog) error{
	"claude": func(*dialog) error { return configure.RunLogin([]string{"claude", "auth", "login"}) },
	"codex":  func(*dialog) error { return configure.RunLogin([]string{"codex", "login"}) },
	"gemini": func(d *dialog) error {
		if !runAgy(d) {
			return errors.New("agy sign-in did not complete")
		}
		return nil
	},
	"opencode": func(*dialog) error {
		bin, err := lookOpencode("opencode")
		if err != nil {
			return err
		}
		cancelled, err := runAttached([]string{bin, "auth", "login"})
		invalidateOpencodeAuth()
		if cancelled {
			return errors.New("sign-in cancelled")
		}
		return err
	},
	"copilot": func(*dialog) error { return configure.RunLogin([]string{"copilot", "login"}) },
}

var (
	setupState = func(d *dialog, agent string) agentState { return agentMenus[agent](d).state }

	setupVerify = configure.VerifyAgent

	setupRoute = routeAgent
)

// setupRow is the page's first row.
func setupRow(d *dialog, agent string) billingRow {
	return billingRow{label: "Set up", desc: "sign in, verify, pin the route", run: func() bool {
		agentSetUp(d, agent)
		return true
	}}
}

// agentSetUp signs in (skipped when already verified), sends one small real request and offers the route pin.
func agentSetUp(d *dialog, agent string) {
	name := displayLabel(agent)
	stop := func(step string, err error) {
		d.printf("%s: set up stopped at %s: %s\n", name, step, shapes.Redact(err.Error()))
	}
	if setupState(d, agent).verified {
		d.printf("%s: already signed in\n", name)
	} else {
		if err := setupLogins[agent](d); err != nil {
			stop("sign-in", err)
			return
		}
		d.changed = true
		agentActiveOn(d, agent)
	}
	if err := setupVerify(context.Background(), agent); err != nil {
		if agent == "copilot" && configure.QuotaFailure(err) {
			err = fmt.Errorf("signed in, but copilot has no credits left, so this is not a login failure (%w)", err)
		}
		stop("verify", err)
		return
	}
	d.printf("%s: verified with one live request\n", name)
	changed, err := setupRoute(d, agent)
	switch {
	case err != nil:
		stop("route", err)
	case !changed:
		d.printf("%s: signed in; the route was not changed\n", name)
	}
}

// routeAgent runs the agent's profile picker; changed is false (err nil) when it was dismissed or left the profile as is.
func routeAgent(d *dialog, agent string) (changed bool, err error) {
	return pickAgentProfile(d, agent, vault.GetAgentRecord(agent).Profile)
}
