package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

// cmdAgentProfile is `<agent> profile [<profile>] [--clear]`: show, set or clear the agent's <AGENT>_PROFILE pin.
func cmdAgentProfile(agent string, args []string) int {
	pin := providers.AgentProfilePin(agent)
	if pin == "" {
		return fail("%s profile: no profile pin for this agent", agent)
	}
	fs := newVendorFlags(agent, "profile")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return fail("%s profile: want one profile name, or --clear", agent)
	}
	switch {
	case *unpin:
		if err := providers.SetPins(map[string]string{pin: ""}); err != nil {
			return fail("%s profile: %v", agent, err)
		}
		fmt.Printf("%s: profile pin cleared\n", agent)
		return 0
	case len(pos) == 0:
		line := fmt.Sprintf("%s: no profile pinned (%s unset; launches use today's selection)", agent, pin)
		if v := providers.Pin(pin); v != "" {
			line = fmt.Sprintf("%s: profile %s (%s)", agent, v, pin)
		}
		if active := providers.AgentActivePin(agent); active != "" {
			state := providers.AgentOn
			if !providers.AgentActive(agent) {
				state = providers.AgentOff
			}
			line += fmt.Sprintf(" · %s (%s)", state, active)
		}
		fmt.Println(line)
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%s profile: %v", agent, err)
	}
	if err := providers.ValidAgentPin(reg, agent, pos[0]); err != nil {
		return fail("%s profile: %v (see claude-code configure provider --list)", agent, err)
	}
	if row := reg.Providers[pos[0]]; row != nil && agent != "claude" && !vendors.CanServe(agent, row) {
		return fail("%s profile: %s cannot serve profile %q", agent, agent, pos[0])
	}
	if err := providers.SetPins(map[string]string{pin: pos[0]}); err != nil {
		return fail("%s profile: %v", agent, err)
	}
	fmt.Printf("%s: now on %s (%s); relaunch `claude-code %s` to apply\n", agent, pos[0], pin, agent)
	preflight.Warm(os.Stdout, reg, time.Now())
	if agent == "codex" {
		fmt.Println("plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return 0
}

// cmdAgentOnOff is `<agent> on|off`: set the agent's <AGENT>_ACTIVE pin; claude cannot be turned off.
func cmdAgentOnOff(agent string, on bool, args []string) int {
	verb, value := "off", providers.AgentOff
	if on {
		verb, value = "on", providers.AgentOn
	}
	fs := newVendorFlags(agent, verb)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return fail("%s %s: takes no arguments", agent, verb)
	}
	pin := providers.AgentActivePin(agent)
	if pin == "" {
		return fail("%s %s: %s is always on", agent, verb, agent)
	}
	if err := providers.ValidAgentActive(agent, value); err != nil {
		return fail("%s %s: %v", agent, verb, err)
	}
	if err := providers.SetPins(map[string]string{pin: value}); err != nil {
		return fail("%s %s: %v", agent, verb, err)
	}
	if on {
		fmt.Printf("%s: on\n", agent)
	} else {
		fmt.Printf("%s: off (%s)\n", agent, pin)
	}
	return 0
}
