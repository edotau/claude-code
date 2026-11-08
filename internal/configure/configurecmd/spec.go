package configurecmd

import (
	"github.com/edotau/claude-code/internal/completion"
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

// sections derives from menu.SectionNames, the package's own run order.
func sections() []string {
	return append(append([]string{}, menu.SectionNames...), "desktop")
}

// Spec is the completion node for the configure verb.
func Spec() *completion.Node {
	formatFlag := completion.ValuedFlag("format", func() []string { return []string{helptext.Text, helptext.JSON} })
	return &completion.Node{Args: []func() []string{sections, providerNames}, Flags: []completion.Flag{
		completion.ValuedFlag("profile", providerNames), completion.ValuedFlag("agent", agentNames), completion.BoolFlag("dry-run"), completion.BoolFlag("check"),
		completion.BoolFlag("restore"), completion.BoolFlag("revert"), completion.Plain("out"), completion.BoolFlag("agents"), completion.BoolFlag("list"), completion.BoolFlag("all"),
		completion.BoolFlag("no-agents"), formatFlag}}
}
