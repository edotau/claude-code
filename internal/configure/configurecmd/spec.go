package configurecmd

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
)

func providerNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

// agentNames are the agent CLIs `configure --agent` can scope to: the agents plus claude itself.
func agentNames() []string { return []string{"claude", "codex", "gemini", "opencode", "copilot"} }

// authNames are the github profiles with a login, plus "follow", for `configure github --auth`.
func authNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return []string{"follow"}
	}
	return append([]string{"follow"}, menu.GitHubAuthProfiles(reg)...)
}

// sections derives from menu.SectionNames, the package's own run order.
func sections() []string {
	return append(append([]string{}, menu.SectionNames...), "desktop")
}

// Spec is the completion node for the configure verb.
func Spec() *helptext.Node {
	formatFlag := helptext.ValuedFlag("format", func() []string { return []string{helptext.Text, helptext.JSON} })
	return &helptext.Node{Args: []func() []string{sections, providerNames}, Flags: []helptext.Flag{
		helptext.ValuedFlag("profile", providerNames), helptext.ValuedFlag("agent", agentNames), helptext.ValuedFlag("auth", authNames), helptext.BoolFlag("dry-run"), helptext.BoolFlag("check"),
		helptext.BoolFlag("restore"), helptext.BoolFlag("revert"), helptext.Plain("out"), helptext.BoolFlag("agents"), helptext.BoolFlag("list"), helptext.BoolFlag("all"),
		helptext.BoolFlag("no-agents"), formatFlag}}
}
