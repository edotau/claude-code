package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/theme"
)

func coreSpecNodes() map[string]*completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*completion.Node{
		"clean":    {Flags: []completion.Flag{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}},
		"usage":    {Flags: []completion.Flag{flag("session"), flag("days"), formatFlag(), boolFlag("workflows"), boolFlag("providers"), flag("show")}},
		"describe": {Flags: []completion.Flag{formatFlag()}},
		"help-svg": {Flags: []completion.Flag{flag("out"), flag("check")}},
		"theme": {Args: []func() []string{theme.GradientNames}, Flags: []completion.Flag{boolFlag("palettes"), boolFlag("reset")},
			Sub: map[string]*completion.Node{
				"apply":      {Flags: []completion.Flag{boolFlag("dry-run")}},
				"statusline": {Flags: []completion.Flag{valuedFlag("style", statuslineStyles)}},
				"vscode": {Flags: []completion.Flag{flag("template"), flag("dest"), flag("extensions-dir"),
					flag("keybindings-template"), flag("keybindings-dest"), boolFlag("check"), boolFlag("dry-run")}},
			}},
		"clip":       {Flags: []completion.Flag{boolFlag("serve")}},
		"completion": {Flags: []completion.Flag{boolFlag("install")}},
	}
}

func networkSpecNodes() *completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return &completion.Node{Sub: map[string]*completion.Node{
		"restart": {Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("force"), formatFlag()}},
		"tailscale": {Flags: []completion.Flag{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
			valuedFlag("exit-node", exitNodeActions), valuedFlag("subnet", subnetActions), valuedFlag("route", routeTargets), flag("auth-key"),
			flag("key-env"), boolFlag("advertise-exit"),
			flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
			boolFlag("preauthorized"), flag("description")}},
		"browser": {Flags: []completion.Flag{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames), boolFlag("dry-run"),
			flag("extract"), flag("container"), flag("field"), flag("next"), flag("pages"), boolFlag("scroll"),
			flag("load-more"), flag("max-pages"), flag("key"), flag("o"), boolFlag("ignore-robots")}},
		"websearch": {Flags: []completion.Flag{flag("query"), flag("max-results"), flag("extract")}},
		"mcp":       {Flags: []completion.Flag{formatFlag()}},
		"page":      {Flags: []completion.Flag{flag("addr"), flag("hosts"), flag("days"), boolFlag("dry-run-dispatch")}},
		"view": {Args: []func() []string{webViewAgentIDs}, Flags: []completion.Flag{flag("session"), flag("days"),
			flag("n"), boolFlag("follow"), formatFlag()}},
		"remote": {Flags: []completion.Flag{valuedFlag("host", tailscale.CachedMachineNames), flag("dir"), valuedFlag("agent", func() []string { return tailscale.RemoteAgents }),
			flag("name"), boolFlag("dry-run"), boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
		"vscode": {Flags: []completion.Flag{flag("port"), boolFlag("stop"), boolFlag("status"), boolFlag("no-tailscale"),
			boolFlag("funnel"), flag("folder"), formatFlag()}},
	}}
}

func workspaceSpecNodes() map[string]*completion.Node {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*completion.Node{
		"agents": {Sub: map[string]*completion.Node{
			"sync": {Flags: []completion.Flag{valuedFlag("agent", cliAgentNames), boolFlag("dry-run"), boolFlag("check"), boolFlag("no-prune")}},
			"tree": {Flags: []completion.Flag{flag("days"), flag("session"), boolFlag("background"), boolFlag("bg"),
				boolFlag("watch"), flag("interval"), formatFlag()}},
			"reap": {Flags: []completion.Flag{boolFlag("dry-run")}},
		}},
		"install": {Flags: []completion.Flag{boolFlag("dry-run"), boolFlag("verbose"), valuedFlag("tools", toolNames), valuedFlag("setup", setup.Modes)}, Sub: map[string]*completion.Node{"--setup": {Sub: map[string]*completion.Node{
			"adopt":     {Flags: []completion.Flag{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
			"nuke":      {Flags: []completion.Flag{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
			"workspace": {Args: []func() []string{setup.WorkspaceOps}, Flags: []completion.Flag{flag("with"), boolFlag("dry-run")}},
		}}}},
		"test": {Sub: map[string]*completion.Node{"mutate": {Flags: []completion.Flag{flag("run")}}, "ramdisk": {}}},
	}
}

// exitNodeActions are the `network tailscale --exit-node` values.
func exitNodeActions() []string { return []string{"on", "off", "approve"} }

// subnetActions are the `network tailscale --subnet` keyword values (a CIDR list is also accepted).
func subnetActions() []string { return []string{"approve", "off"} }

// routeTargets are the `network tailscale --route` values: dispatch hosts or "off" to clear.
func routeTargets() []string { return append(tailscale.CachedMachineNames(), "off") }

// statuslineStyles are the `theme statusline --style` values.
func statuslineStyles() []string {
	return []string{"gradient", "powerline", "capsule", "minimal", "dashboard"}
}

// toolNames derives from agent.Tools, the install --tools roster.
func toolNames() []string { return tools.Names() }

func workflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// browserNames mirrors `network browser --setup --browser`'s accepted values.
func browserNames() []string {
	return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
}

func webViewAgentIDs() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return transcript.AgentIDs(cwd)
}

func hookEvents() []string { return settings.HookEvents() }
