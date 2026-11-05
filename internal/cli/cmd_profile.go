package cli

import (
	"fmt"
	"os"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// cmdAgentProfile is `<agent> profile [<profile>] [--clear]`: show, set or clear the agent's record profile.
func cmdAgentProfile(agent string, args []string) int {
	if _, ok := agentpkg.LookupSchema(agent); !ok {
		return helptext.Fail("%s profile: unknown agent", agent)
	}
	fs := newAgentFlags(agent, "profile")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return helptext.Fail("%s profile: want one profile name, or --clear", agent)
	}
	switch {
	case *unpin:
		own := vault.OwnLoginProfiles[agent] // an external agent's cleared profile is its own login, not off
		if _, err := configure.ApplyProfile([]string{agent}, own); err != nil {
			return helptext.Fail("%s profile: %v", agent, err)
		}
		fmt.Printf("%s: profile cleared (own login)\n", agent)
		return 0
	case len(pos) == 0:
		fmt.Println(configure.ProfileStatus(agent))
		return 0
	}
	return setAgentProfile(agent, pos[0])
}

func setAgentProfile(agent, profile string) int {
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%s profile: %v", agent, err)
	}
	if _, err := configure.ApplyProfile([]string{agent}, profile); err != nil {
		return helptext.Fail("%s profile: %v (see claude-code configure provider --list)", agent, err)
	}
	fmt.Printf("%s: now on %s; relaunch `claude-code %s` to apply\n", agent, profile, agent)
	preflight.Warm(os.Stdout, reg, time.Now())
	if agent == "codex" {
		fmt.Println("plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return 0
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
		return helptext.Fail("%s %s: takes no arguments", agent, verb)
	}
	if agentpkg.VaultAgentIsClaude(agent) {
		return helptext.Fail("%s %s: %s is always on", agent, verb, agent)
	}
	if err := agentpkg.ValidAgentActive(agent, value); err != nil {
		return helptext.Fail("%s %s: %v", agent, verb, err)
	}
	profile := ""
	if on {
		profile = vault.OwnLoginProfiles[agent]
	}
	if _, err := configure.ApplyProfile([]string{agent}, profile); err != nil {
		return helptext.Fail("%s %s: %v", agent, verb, err)
	}
	if on {
		fmt.Printf("%s: on\n", agent)
	} else {
		fmt.Printf("%s: off\n", agent)
	}
	return 0
}
