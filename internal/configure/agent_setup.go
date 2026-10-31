// agent_setup.go — the Set up row every agent page opens with: sign in, verify, pin the route; stops at the first failure.
package configure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// setupVerifyTimeout caps the one small real request the verify step sends.
const setupVerifyTimeout = 2 * time.Minute

// setupLogins is each agent's existing sign-in path; an agent absent here has no Set up row.
var setupLogins = map[string]func(d *dialog) error{
	"claude": func(*dialog) error { return runLogin([]string{"claude", "auth", "login"}) },
	"codex":  func(*dialog) error { return runLogin([]string{"codex", "login"}) },
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
	"copilot": func(*dialog) error { return runLogin([]string{"copilot", "login"}) },
}

// Seams: tests replace them so no login, agent call or terminal is touched.
var (
	setupState  = func(d *dialog, agent string) agentState { return agentMenus[agent](d).state }
	setupVerify = verifyAgent
	setupRoute  = routeAgent
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
		if agent == "copilot" && quotaFailure(err) {
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

// quotaFailure reports a verify error that means exhausted credits or a rate limit, not bad credentials.
func quotaFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "quota") || strings.Contains(s, "rate_limit")
}

// verifyAgent asks the agent for "pong" through `claude-code ask`, the same request preflight sends.
func verifyAgent(ctx context.Context, agent string) error {
	bin, err := agentpkg.SelfBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, setupVerifyTimeout)
	defer cancel()
	pin := preflight.Pin{Agent: agent, Status: preflight.StatusOK, Served: "n/a"}
	for _, p := range preflight.LivePongs(ctx, bin, []preflight.Pin{pin}) {
		if !p.OK {
			return errors.New(p.Err)
		}
	}
	return nil
}

// routeAgent runs the agent's route picker; changed is false (err nil) when it was dismissed or left the pin as is.
func routeAgent(d *dialog, agent string) (changed bool, err error) {
	key := agentpkg.AgentProfilePin(agent)
	if key == "" {
		return false, errors.New("no route pin for this agent")
	}
	return pickAgentRoute(d, agent, key, providers.Pin(key))
}
