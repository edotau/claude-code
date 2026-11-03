package cli

import (
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/worktree"
)

// formatValues is the shared text|json domain almost every --format flag offers.
func formatValues() []string { return []string{helptext.Text, helptext.JSON} }

func setupWorkspaceSubwords() []string { return []string{"init", "link", "status"} }

func setupModes() []string { return []string{"adopt", "nuke", "workspace"} }

func cliAgentNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// toolNames derives from agent.Tools, the install --tools roster.
func toolNames() []string { return tools.Names() }

// configureAgentNames are the agent CLIs `configure --agent` can scope to: the agents plus claude itself.
func configureAgentNames() []string { return append([]string{"claude"}, cliAgentNames()...) }

func workflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// reviewToolNames derives from reviewTools (review.go) so a new detector is completable for free.
func reviewToolNames() []string {
	out := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// configureSections derives from menu.SectionNames, the package's own run order.
func configureSections() []string {
	return append(append([]string{}, menu.SectionNames...), "desktop")
}

// modelPinAliases offers "alias=" stems for --pin; the user types the model id after the "=" themselves.
func modelPinAliases() []string {
	out := make([]string, 0, len(models.AliasNames)+1)
	for _, s := range models.AliasNames {
		out = append(out, s+"=")
	}
	return append(out, "all=")
}

// browserNames mirrors `network browser --setup --browser`'s accepted values.
func browserNames() []string {
	return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
}

// providerNames reads the provider registry straight off disk (no network): the same file
// `claude-code configure provider --list`/`switch` already read.
func providerNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

// agentNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never
// dials an agent.
func agentNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}

func secretNames() []string { return providers.SecretNames() }

func memoryRepoNames() []string { return memstore.RepoKeys() }

func worktreeNames() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return worktree.Names(cwd)
}

func webViewAgentIDs() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return transcript.AgentIDs(cwd)
}

func hookEvents() []string { return settings.HookEvents() }
