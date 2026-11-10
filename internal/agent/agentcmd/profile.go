package agentcmd

import (
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// agentProfile is `<agent> profile [<profile>] [--clear]`: show, set or clear the agent's record profile.
func agentProfile(name string, args []string) int {
	if _, ok := agent.LookupSchema(name); !ok {
		return helptext.Fail("%s profile: unknown agent", name)
	}
	fs := newAgentFlags(name, "profile")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return helptext.Fail("%s profile: want one profile name, or --clear", name)
	}
	switch {
	case *unpin:
		own := vault.OwnLoginProfiles[name] // an external agent's cleared profile is its own login, not off
		if _, err := configure.ApplyProfile([]string{name}, own); err != nil {
			return helptext.Fail("%s profile: %v", name, err)
		}
		fmt.Printf("%s: profile cleared (own login)\n", name)
		return 0
	case len(pos) == 0:
		fmt.Println(configure.ProfileStatus(name))
		return 0
	}
	return setAgentProfile(name, pos[0])
}

func setAgentProfile(name, profile string) int {
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%s profile: %v", name, err)
	}
	if _, err := configure.ApplyProfile([]string{name}, profile); err != nil {
		return helptext.Fail("%s profile: %v (see claude-code configure provider --list)", name, err)
	}
	if p := reg.Providers[profile]; name == "copilot" && p != nil && p.Provider == "github" && p.Login == "" {
		fmt.Printf("copilot: %s has no login; billing follows gh's active account (set one with `vault edit`)\n", profile)
	}
	fmt.Printf("%s: now on %s; relaunch `claude-code %s` to apply\n", name, profile, name)
	preflight.Warm(os.Stdout, reg, time.Now())
	if name == "codex" {
		fmt.Println("plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return 0
}

// agentOnOff is `<agent> on|off`: off clears the record's profile, on sets the own login; claude is always on.
func agentOnOff(name string, on bool, args []string) int {
	verb, value := "off", agent.AgentOff
	if on {
		verb, value = "on", agent.AgentOn
	}
	fs := newAgentFlags(name, verb)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.Fail("%s %s: takes no arguments", name, verb)
	}
	if agent.VaultAgentIsClaude(name) {
		return helptext.Fail("%s %s: %s is always on", name, verb, name)
	}
	if err := agent.ValidAgentActive(name, value); err != nil {
		return helptext.Fail("%s %s: %v", name, verb, err)
	}
	profile := ""
	if on {
		profile = vault.OwnLoginProfiles[name]
	}
	if _, err := configure.ApplyProfile([]string{name}, profile); err != nil {
		return helptext.Fail("%s %s: %v", name, verb, err)
	}
	if on {
		fmt.Printf("%s: on\n", name)
	} else {
		fmt.Printf("%s: off\n", name)
	}
	return 0
}
