package cli

import (
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
)

func askSpecFlags() []flagInfo {
	flag, boolFlag, valuedFlag := specFlag, specBoolFlag, specValuedFlag
	return []flagInfo{
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
func agentsRunSpecNodes(askFlags []flagInfo) map[string]*compNode {
	flag, boolFlag, formatFlag := specFlag, specBoolFlag, specFormatFlag
	return map[string]*compNode{
		"list": {flags: []flagInfo{flag("host"), flag("remote-bin"), formatFlag()}},
		"ask":  {flags: askFlags},
		"dispatch": {args: []func() []string{func() []string { return []string{"explore", "fleet", "compare"} }}, flags: []flagInfo{
			flag("part"), boolFlag("no-web"), flag("models"), flag("jobs"), flag("timeout"), formatFlag(), boolFlag("background")}},
	}
}

func addLauncherSpecNodes(tree map[string]*compNode, askFlags []flagInfo) {
	flag, boolFlag := specFlag, specBoolFlag
	// Launchers that only ever get a bare node (no flags of their own — see noComplete).
	for _, v := range []string{"opus", "sonnet", "haiku", "fable", "science"} {
		tree[v] = &compNode{}
	}
	tree["anthropic"] = &compNode{flags: []flagInfo{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"),
		boolFlag("print-env"), flag("keep-env"), flag("overlay")}}
	// Agent verbs: ask/resume/review/transfer/status/result/cancel/setup/catalog, same shape per agent.
	for _, v := range []string{"codex", "copilot", "opencode"} {
		tree[v] = &compNode{sub: agentSubNodes(askFlags)}
	}
	tree["gemini"] = &compNode{sub: agentSubNodes(askFlags)}
	// Every remaining verb (hook/statusline/version/completion/__complete and anything future) at
	// least gets an empty node so it's never silently unrepresented.
	for _, c := range commands() {
		if tree[c.name] == nil {
			tree[c.name] = &compNode{}
		}
	}
	tree["hook"] = &compNode{args: []func() []string{hookEvents}}
}

// agentSubNodes is the one builder for every agent verb's completion subtree (codex, copilot,
// opencode, gemini): ask/resume/review/transfer/status/result/cancel/setup/catalog, same shape per agent.
func agentSubNodes(askFlags []flagInfo) map[string]*compNode {
	flag := func(name string) flagInfo { return flagInfo{name: name} }
	boolFlag := func(name string) flagInfo { return flagInfo{name: name, isBool: true} }
	formatFlag := func() flagInfo { return flagInfo{name: "format", values: formatValues} }
	return map[string]*compNode{
		"ask":    {flags: askFlags},
		"resume": {},
		"review": {flags: []flagInfo{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric")}},
		"explore":  {flags: []flagInfo{boolFlag("no-web"), boolFlag("background"), formatFlag()}},
		"transfer": {flags: []flagInfo{flag("source"), boolFlag("wait")}},
		"status":   {flags: []flagInfo{boolFlag("all"), formatFlag()}},
		"tail":     {flags: []flagInfo{boolFlag("f")}},
		"result":   {flags: []flagInfo{formatFlag()}},
		"cancel":   {},
		"profile":  {args: []func() []string{providerNames}, flags: []flagInfo{boolFlag("clear")}},
		"on":       {},
		"off":      {},
		"setup":    {flags: []flagInfo{formatFlag()}},
		"catalog":  {},
	}
}

// reviewSubNodes gives every review detector (reviewTools, review.go) an empty node — none of them
// have their own completable flags today.
func reviewSubNodes() map[string]*compNode {
	out := map[string]*compNode{}
	for _, n := range reviewToolNames() {
		out[n] = &compNode{}
	}
	return out
}

func cliAgentNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// reviewToolNames derives from reviewTools (review.go) so a new detector is completable for free.
func reviewToolNames() []string {
	out := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
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
