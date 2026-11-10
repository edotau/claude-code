package agentcmd

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// VendorNames lists the agent CLIs that have their own verb.
func VendorNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

// EngineNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never dials an agent.
func EngineNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}

func askSpecFlags() []helptext.Flag {
	flag, boolFlag, valuedFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag
	return []helptext.Flag{
		valuedFlag("agent", EngineNames), flag("agents"), valuedFlag("profile", providers.ProfileNames), flag("model"),
		flag("effort"), flag("workdir"), flag("session"), boolFlag("tools"), boolFlag("read-only"), boolFlag("no-tools"), boolFlag("native"), flag("allow"), flag("ask"), flag("deny"),
		valuedFlag("mode", func() []string { return []string{"default", "auto", "plan"} }),
		flag("timeout"), flag("format"), flag("rubric"), flag("retry"), boolFlag("log"),
		valuedFlag("fallback", func() []string { return router.FallbackModes }),
		flag("attach"), flag("keep-env"), flag("overlay"), boolFlag("autonomous"), boolFlag("fleet"), boolFlag("enable-memory"),
		boolFlag("background"), flag("job"), flag("batch"), flag("jobs"), boolFlag("fail-fast"),
		boolFlag("no-persist"), boolFlag("safe-mode"), flag("debug-file"), valuedFlag("as", func() []string { return []string{"user", "sp"} }),
	}
}

// AgentsSpec is the completion node for the agents verb.
func AgentsSpec() *helptext.Node {
	flag, boolFlag, valuedFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"sync": {Flags: []helptext.Flag{valuedFlag("agent", VendorNames), boolFlag("dry-run"), boolFlag("check"), boolFlag("no-prune")}},
		"tree": {Flags: []helptext.Flag{flag("days"), flag("session"), boolFlag("background"), boolFlag("bg"),
			boolFlag("watch"), flag("interval"), formatFlag()}},
		"reap":  {Flags: []helptext.Flag{boolFlag("dry-run")}},
		"label": {Flags: []helptext.Flag{flag("model"), flag("brief"), flag("session-model"), formatFlag()}},
		"list":  {Flags: []helptext.Flag{flag("host"), flag("remote-bin"), formatFlag()}},
		"ask":   {Flags: askSpecFlags()},
		"dispatch": {Args: []func() []string{func() []string { return []string{"explore", "fleet", "compare"} }}, Flags: []helptext.Flag{
			flag("part"), boolFlag("no-web"), flag("models"), flag("jobs"), flag("timeout"), formatFlag(), boolFlag("background")}},
	}}
}

// AnthropicSpec is the completion node for the anthropic verb.
func AnthropicSpec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{"science": {}},
		Flags: []helptext.Flag{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"), flag("profile"),
			boolFlag("print-env"), flag("keep-env"), flag("overlay")}}
}

// HookSpec is the completion node for the hook verb.
func HookSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{settings.HookEvents}}
}

// AgentSpec is the one completion subtree for every agent verb (codex, copilot, opencode, gemini).
func AgentSpec() *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ask":    {Flags: askSpecFlags()},
		"resume": {},
		"review": {Flags: []helptext.Flag{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric")}},
		"explore":  {Flags: []helptext.Flag{boolFlag("no-web"), boolFlag("background"), formatFlag()}},
		"transfer": {Flags: []helptext.Flag{flag("source"), boolFlag("wait")}},
		"status":   {Flags: []helptext.Flag{boolFlag("all"), formatFlag()}},
		"tail":     {Flags: []helptext.Flag{boolFlag("f")}},
		"result":   {Flags: []helptext.Flag{formatFlag()}},
		"cancel":   {},
		"profile":  {Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{boolFlag("clear")}},
		"on":       {},
		"off":      {},
		"setup":    {Flags: []helptext.Flag{formatFlag()}},
		"catalog":  {},
	}}
}
