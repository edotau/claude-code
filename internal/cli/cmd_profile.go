package cli

import (
	"fmt"
	"os"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// cmdAgentProfile is `<agent> profile [<profile>] [--clear]`: show, set or clear the agent's record profile.
func cmdAgentProfile(agent string, args []string) int {
	pin := agentpkg.AgentProfilePin(agent)
	if pin == "" {
		return fail("%s profile: no profile pin for this agent", agent)
	}
	fs := newAgentFlags(agent, "profile")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return fail("%s profile: want one profile name, or --clear", agent)
	}
	switch {
	case *unpin:
		own := vault.OwnLoginProfiles[agent] // an external agent's cleared profile is its own login, not off
		if err := setAgentProfile(agent, own); err != nil {
			return fail("%s profile: %v", agent, err)
		}
		if err := agentpkg.ClearLastProfilesFor(map[string]string{pin: ""}); err != nil {
			return fail("%s profile: %v", agent, err)
		}
		fmt.Printf("%s: profile cleared (own login)\n", agent)
		return 0
	case len(pos) == 0:
		line := fmt.Sprintf("%s: no profile set (launches use today's selection)", agent)
		if v := agentpkg.GetAgentRecord(agent).Profile; v != "" {
			line = fmt.Sprintf("%s: profile %s", agent, v)
		}
		if active := agentpkg.AgentActivePin(agent); active != "" {
			state := agentpkg.AgentOn
			if !agentpkg.AgentActive(agent) {
				state = agentpkg.AgentOff
			} else if why := agentpkg.AgentUnbillable(agent); why != "" {
				state = agentpkg.AgentOff + ", " + why
			}
			line += " · " + state
		}
		fmt.Println(line)
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%s profile: %v", agent, err)
	}
	if err := agentpkg.ValidAgentPin(reg, agent, pos[0]); err != nil {
		return fail("%s profile: %v (see claude-code configure provider --list)", agent, err)
	}
	if row := reg.Providers[pos[0]]; row != nil && agent != "claude" && !agentpkg.CanServe(agent, row) {
		return fail("%s profile: %s cannot serve profile %q", agent, agent, pos[0])
	}
	if err := setAgentProfile(agent, pos[0]); err != nil {
		return fail("%s profile: %v", agent, err)
	}
	if err := agentpkg.ClearLastProfilesFor(map[string]string{pin: pos[0]}); err != nil {
		return fail("%s profile: %v", agent, err)
	}
	fmt.Printf("%s: now on %s; relaunch `claude-code %s` to apply\n", agent, pos[0], agent)
	preflight.Warm(os.Stdout, reg, time.Now())
	if agent == "codex" {
		fmt.Println("plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return 0
}

// setAgentProfile writes profile into agent's record, leaving its other fields.
func setAgentProfile(agent, profile string) error {
	return agentpkg.UpdateAgentRecords(func(m map[string]agentpkg.AgentRecord) {
		r := m[agent]
		r.Profile = profile
		m[agent] = r
	})
}

// cmdAgentOnOff is `<agent> on|off`: off clears the record's profile, on sets the own login; claude is always on.
func cmdAgentOnOff(agent string, on bool, args []string) int {
	verb, value := "off", agentpkg.AgentOff
	if on {
		verb, value = "on", agentpkg.AgentOn
	}
	fs := newAgentFlags(agent, verb)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return fail("%s %s: takes no arguments", agent, verb)
	}
	pin := agentpkg.AgentActivePin(agent)
	if pin == "" {
		return fail("%s %s: %s is always on", agent, verb, agent)
	}
	if err := agentpkg.ValidAgentActive(agent, value); err != nil {
		return fail("%s %s: %v", agent, verb, err)
	}
	profile := ""
	if on {
		profile = vault.OwnLoginProfiles[agent]
	}
	if err := setAgentProfile(agent, profile); err != nil {
		return fail("%s %s: %v", agent, verb, err)
	}
	if on {
		fmt.Printf("%s: on\n", agent)
	} else {
		fmt.Printf("%s: off\n", agent)
	}
	return 0
}
