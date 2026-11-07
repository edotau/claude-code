package configurecmd

import (
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
)

func plainFlag(name string) completion.Flag { return completion.Flag{Name: name} }
func boolFlag(name string) completion.Flag  { return completion.Flag{Name: name, IsBool: true} }

func valuedFlag(name string, values func() []string) completion.Flag {
	return completion.Flag{Name: name, Values: values}
}

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
	formatFlag := valuedFlag("format", func() []string { return []string{helptext.Text, helptext.JSON} })
	return &completion.Node{Args: []func() []string{sections, providerNames}, Flags: []completion.Flag{
		valuedFlag("profile", providerNames), valuedFlag("agent", agentNames), boolFlag("dry-run"), boolFlag("check"),
		boolFlag("restore"), boolFlag("revert"), plainFlag("out"), boolFlag("agents"), boolFlag("list"), boolFlag("all"),
		boolFlag("no-agents"), formatFlag}}
}
