package cli

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/router"
)

func askSpecFlags() []completion.Flag {
	flag, boolFlag, valuedFlag := specFlag, specBoolFlag, specValuedFlag
	return []completion.Flag{
		valuedFlag("agent", agentNames), flag("agents"), valuedFlag("profile", providerNames), flag("model"),
		flag("effort"), flag("workdir"), flag("session"), boolFlag("tools"), boolFlag("read-only"), boolFlag("no-tools"), boolFlag("native"), flag("allow"), flag("ask"), flag("deny"),
		valuedFlag("mode", func() []string { return []string{"default", "auto", "plan"} }),
		flag("timeout"), flag("format"), flag("rubric"), flag("retry"), boolFlag("log"),
		valuedFlag("fallback", func() []string { return router.FallbackModes }),
		flag("attach"), flag("keep-env"), flag("overlay"), boolFlag("autonomous"), boolFlag("fleet"), boolFlag("enable-memory"),
		boolFlag("background"), flag("job"), flag("batch"), flag("jobs"), boolFlag("fail-fast"),
		boolFlag("no-persist"), boolFlag("safe-mode"), flag("debug-file"), valuedFlag("as", func() []string { return []string{"user", "sp"} }),
	}
}

// agentsRunSpecNodes are the `agents` subcommands that run a task: list, ask and dispatch.
func agentsRunSpecNodes(askFlags []completion.Flag) map[string]*completion.Node {
	flag, boolFlag, formatFlag := specFlag, specBoolFlag, specFormatFlag
	return map[string]*completion.Node{
		"list": {Flags: []completion.Flag{flag("host"), flag("remote-bin"), formatFlag()}},
		"ask":  {Flags: askFlags},
		"dispatch": {Args: []func() []string{func() []string { return []string{"explore", "fleet", "compare"} }}, Flags: []completion.Flag{
			flag("part"), boolFlag("no-web"), flag("models"), flag("jobs"), flag("timeout"), formatFlag(), boolFlag("background")}},
	}
}

func addLauncherSpecNodes(tree map[string]*completion.Node, askFlags []completion.Flag) {
	flag, boolFlag := specFlag, specBoolFlag
	// Launchers that only ever get a bare node (no flags of their own — see noComplete).
	for _, v := range []string{"opus", "sonnet", "haiku", "fable", "science"} {
		tree[v] = &completion.Node{}
	}
	tree["anthropic"] = &completion.Node{Flags: []completion.Flag{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"),
		boolFlag("print-env"), flag("keep-env"), flag("overlay")}}
	// Agent verbs: ask/resume/review/transfer/status/result/cancel/setup/catalog, same shape per agent.
	for _, v := range []string{"codex", "copilot", "opencode"} {
		tree[v] = &completion.Node{Sub: agentSubNodes(askFlags)}
	}
	tree["gemini"] = &completion.Node{Sub: agentSubNodes(askFlags)}
	// Every remaining verb (hook/statusline/version/completion/__complete and anything future) at
	// least gets an empty node so it's never silently unrepresented.
	for _, c := range commands() {
		if tree[c.name] == nil {
			tree[c.name] = &completion.Node{}
		}
	}
	tree["hook"] = &completion.Node{Args: []func() []string{hookEvents}}
}

// agentSubNodes is the one builder for every agent verb's completion subtree (codex, copilot,
// opencode, gemini): ask/resume/review/transfer/status/result/cancel/setup/catalog, same shape per agent.
func agentSubNodes(askFlags []completion.Flag) map[string]*completion.Node {
	flag := func(name string) completion.Flag { return completion.Flag{Name: name} }
	boolFlag := func(name string) completion.Flag { return completion.Flag{Name: name, IsBool: true} }
	formatFlag := func() completion.Flag { return completion.Flag{Name: "format", Values: formatValues} }
	return map[string]*completion.Node{
		"ask":    {Flags: askFlags},
		"resume": {},
		"review": {Flags: []completion.Flag{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric")}},
		"explore":  {Flags: []completion.Flag{boolFlag("no-web"), boolFlag("background"), formatFlag()}},
		"transfer": {Flags: []completion.Flag{flag("source"), boolFlag("wait")}},
		"status":   {Flags: []completion.Flag{boolFlag("all"), formatFlag()}},
		"tail":     {Flags: []completion.Flag{boolFlag("f")}},
		"result":   {Flags: []completion.Flag{formatFlag()}},
		"cancel":   {},
		"profile":  {Args: []func() []string{providerNames}, Flags: []completion.Flag{boolFlag("clear")}},
		"on":       {},
		"off":      {},
		"setup":    {Flags: []completion.Flag{formatFlag()}},
		"catalog":  {},
	}
}

func cliAgentNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// agentNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never
// dials an agent.
func agentNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}
