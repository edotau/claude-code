package cli

import (
	"fmt"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
)

// agentProfilePins maps each agent to the vault pin holding its profile.
var agentProfilePins = map[string]string{
	"codex": providers.PinCodexProfile, "copilot": providers.PinCopilotProfile,
	"gemini": providers.PinGeminiProfile, "opencode": providers.PinOpencodeProfile,
}

// cmdAgentEnv is `<agent> env [<profile>] [--clear]`: show, set or clear the agent's <AGENT>_PROFILE pin.
func cmdAgentEnv(agent string, args []string) int {
	pin := agentProfilePins[agent]
	if pin == "" {
		return fail("%s env: no profile pin for this agent", agent)
	}
	fs := newVendorFlags(agent, "env")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return fail("%s env: want one profile name, or --clear", agent)
	}
	switch {
	case *unpin:
		if err := providers.SetPins(map[string]string{pin: ""}); err != nil {
			return fail("%s env: %v", agent, err)
		}
		fmt.Printf("%s: profile pin cleared\n", agent)
		return 0
	case len(pos) == 0:
		if v := providers.Pin(pin); v != "" {
			fmt.Printf("%s: %s (%s)\n", agent, v, pin)
		} else {
			fmt.Printf("%s: no profile pinned (%s unset; launches use today's selection)\n", agent, pin)
		}
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%s env: %v", agent, err)
	}
	if _, ok := reg.Providers[pos[0]]; !ok {
		return fail("%s env: unknown profile %q (see claude-code configure provider --list)", agent, pos[0])
	}
	if err := providers.SetPins(map[string]string{pin: pos[0]}); err != nil {
		return fail("%s env: %v", agent, err)
	}
	fmt.Printf("%s: now on %s (%s); relaunch `claude-code %s` to apply\n", agent, pos[0], pin, agent)
	if agent == "codex" {
		fmt.Println("plain codex: run `claude-code configure --reset --agents` to re-render config.toml")
	}
	return 0
}
