package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/theme"
)

func coreSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"resolve":  {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("proxy")}},
		"switch":   {args: []func() []string{providerNames}, flags: []flagInfo{boolFlag("dry-run")}},
		"clean":    {flags: []flagInfo{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}},
		"usage":    {flags: []flagInfo{flag("session"), flag("days"), formatFlag(), boolFlag("workflows"), boolFlag("providers"), flag("show")}},
		"describe": {flags: []flagInfo{formatFlag()}},
		"doctor":   {flags: []flagInfo{boolFlag("audit"), flag("days"), boolFlag("scan"), boolFlag("git"), flag("since"), flag("range"), boolFlag("secrets"), boolFlag("refresh"), formatFlag()}},
		"help-svg": {flags: []flagInfo{flag("out"), flag("check")}},
		"theme": {args: []func() []string{theme.GradientNames}, flags: []flagInfo{boolFlag("palettes"), boolFlag("reset")},
			sub: map[string]*compNode{
				"apply":      {flags: []flagInfo{boolFlag("dry-run")}},
				"statusline": {flags: []flagInfo{valuedFlag("style", statuslineStyles)}},
				"vscode": {flags: []flagInfo{flag("template"), flag("dest"), flag("extensions-dir"),
					flag("keybindings-template"), flag("keybindings-dest"), boolFlag("check"), boolFlag("dry-run")}},
			}},
		"configure": {args: []func() []string{configureSections, providerNames}, flags: []flagInfo{
			valuedFlag("profile", providerNames), valuedFlag("agent", configureAgentNames), boolFlag("dry-run"), boolFlag("check"),
			boolFlag("restore"), boolFlag("revert"), flag("out"), boolFlag("agents"), boolFlag("list"), boolFlag("all"),
			boolFlag("no-agents"), formatFlag()}},
		"preflight":  {flags: []flagInfo{flag("discover"), boolFlag("live"), boolFlag("refresh")}},
		"clip":       {flags: []flagInfo{boolFlag("serve")}},
		"completion": {flags: []flagInfo{boolFlag("install")}},
	}
}

func networkSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"network": {sub: map[string]*compNode{
			"restart": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("force"), formatFlag()}},
			"tailscale": {flags: []flagInfo{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
				valuedFlag("exit-node", exitNodeActions), valuedFlag("subnet", subnetActions), valuedFlag("route", routeTargets), flag("auth-key"),
				flag("key-env"), boolFlag("advertise-exit"),
				flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
				boolFlag("preauthorized"), flag("description")}},
			"browser": {flags: []flagInfo{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames), boolFlag("dry-run"),
				flag("extract"), flag("container"), flag("field"), flag("next"), flag("pages"), boolFlag("scroll"),
				flag("load-more"), flag("max-pages"), flag("key"), flag("o"), boolFlag("ignore-robots")}},
			"websearch": {flags: []flagInfo{flag("query"), flag("max-results"), flag("extract")}},
			"mcp":       {flags: []flagInfo{formatFlag()}},
			"page":      {flags: []flagInfo{flag("addr"), flag("hosts"), flag("days"), boolFlag("dry-run-dispatch")}},
			"view": {args: []func() []string{webViewAgentIDs}, flags: []flagInfo{flag("session"), flag("days"),
				flag("n"), boolFlag("follow"), formatFlag()}},
			"remote": {flags: []flagInfo{valuedFlag("host", tailscale.CachedMachineNames), flag("dir"), valuedFlag("agent", func() []string { return tailscale.RemoteAgents }),
				flag("name"), boolFlag("dry-run"), boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
			"vscode": {flags: []flagInfo{flag("port"), boolFlag("stop"), boolFlag("status"), boolFlag("no-tailscale"),
				boolFlag("funnel"), flag("folder"), formatFlag()}},
		}},
	}
}

func workspaceSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"history": {sub: map[string]*compNode{
			"snapshot": {flags: []flagInfo{flag("note")}},
			"log":      {flags: []flagInfo{flag("n"), formatFlag()}},
			"diff":     {},
		}},
		"agents": {sub: map[string]*compNode{
			"sync": {flags: []flagInfo{valuedFlag("agent", cliAgentNames), boolFlag("dry-run"), boolFlag("check"), boolFlag("no-prune")}},
			"tree": {flags: []flagInfo{flag("days"), flag("session"), boolFlag("background"), boolFlag("bg"),
				boolFlag("watch"), flag("interval"), formatFlag()}},
			"reap": {flags: []flagInfo{boolFlag("dry-run")}},
		}},
		"install": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("verbose"), valuedFlag("tools", toolNames), valuedFlag("setup", setupModes)}, sub: map[string]*compNode{"--setup": {sub: map[string]*compNode{
			"adopt":     {flags: []flagInfo{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
			"nuke":      {flags: []flagInfo{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
			"workspace": {args: []func() []string{setupWorkspaceSubwords}, flags: []flagInfo{flag("with"), boolFlag("dry-run")}},
		}}}},
		"worktree": {sub: map[string]*compNode{
			"list":   {flags: []flagInfo{formatFlag()}},
			"create": {flags: []flagInfo{flag("branch")}},
			"merge": {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run"), flag("target"),
				boolFlag("no-clean"), valuedFlag("resolve-agent", worktreeResolveAgents), boolFlag("resume"), flag("verify"), boolFlag("no-verify"), boolFlag("full-verify")}},
			"clean":     {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run"), boolFlag("force")}},
			"clean-all": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("yes")}},
		}},
		"test": {sub: map[string]*compNode{"mutate": {flags: []flagInfo{flag("run")}}, "ramdisk": {}}},
		"plan": {sub: map[string]*compNode{
			"status": {flags: []flagInfo{boolFlag("all"), formatFlag()}},
			"tick":   {flags: []flagInfo{flag("range"), flag("plan"), boolFlag("dry-run")}},
		}},
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

func setupWorkspaceSubwords() []string { return []string{"init", "link", "status"} }

func setupModes() []string { return []string{"adopt", "nuke", "workspace"} }

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
