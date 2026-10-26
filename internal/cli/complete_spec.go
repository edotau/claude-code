package cli

import (
	"github.com/edotau/claude-code/internal/providers"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/statusline"
)

// specTree returns the one completion spec tree, keyed by each verb's canonical name (command.name —
// see knownPath). Built once per process (buildSpecTree) since tab completion is hot and the tree
// never changes after commands() is fixed at startup.
func specTree() map[string]*compNode {
	specTreeInit.Do(func() { specTreeCache = buildSpecTree() })
	return specTreeCache
}

var (
	specTreeInit  sync.Once
	specTreeCache map[string]*compNode
)

// buildSpecTree does the one-time work specTree caches. Every verb in commands() gets a node, even
// an empty one, so a launcher is never silently unrepresented. Subcommand words are hand data
// (mirroring each verb's own dispatch switch, same as before); the three former hand tables
// (subVerbs/positionals/flagsFor) are gone — this is the one place they're declared, nested instead
// of parallel.
func buildSpecTree() map[string]*compNode {
	flag := func(name string) flagInfo { return flagInfo{name: name} }
	boolFlag := func(name string) flagInfo { return flagInfo{name: name, isBool: true} }
	valuedFlag := func(name string, values func() []string) flagInfo { return flagInfo{name: name, values: values} }
	formatFlag := func() flagInfo { return valuedFlag("format", formatValues) }
	// gateway route's flags, which bare `gateway` also takes.
	gatewayRouteFlags := func() []flagInfo {
		return []flagInfo{valuedFlag("tier", func() []string { return route.Tiers }),
			valuedFlag("agent", func() []string { return route.Runners }),
			flag("task"), valuedFlag("fallback", func() []string { return agent.FallbackModes }), formatFlag()}
	}
	vaultSections := func() []string {
		return []string{"secrets", "pins", "env", "providers", "vendors", "settings", "vault", "all"}
	}

	askFlags := []flagInfo{
		valuedFlag("agent", agentNames), valuedFlag("provider", providerNames), flag("model"),
		flag("effort"), flag("workdir"), flag("session"), boolFlag("tools"), boolFlag("native"),
		flag("timeout"), flag("format"), flag("rubric"), flag("retry"), boolFlag("log"),
		valuedFlag("fallback", func() []string { return agent.FallbackModes }),
		flag("attach"), flag("keep-env"), boolFlag("autonomous"), boolFlag("enable-memory"),
		boolFlag("background"), flag("job"),
	}

	tree := map[string]*compNode{
		"resolve":  {flags: []flagInfo{valuedFlag("provider", providerNames), boolFlag("router")}},
		"switch":   {args: []func() []string{providerNames}, flags: []flagInfo{boolFlag("dry-run")}},
		"clean":    {flags: []flagInfo{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}},
		"usage":    {flags: []flagInfo{flag("session"), flag("days"), formatFlag(), boolFlag("workflows"), boolFlag("providers")}},
		"describe": {flags: []flagInfo{formatFlag()}},
		"doctor":   {flags: []flagInfo{boolFlag("audit"), flag("days"), boolFlag("scan"), boolFlag("git"), flag("since"), flag("range"), boolFlag("secrets"), formatFlag()}},
		"help-svg": {flags: []flagInfo{flag("out"), flag("check")}},
		"ask":      {flags: askFlags},
		"theme": {args: []func() []string{statusline.GradientNames}, flags: []flagInfo{boolFlag("preview")},
			sub: map[string]*compNode{
				"apply":      {flags: []flagInfo{boolFlag("dry-run")}},
				"statusline": {flags: []flagInfo{valuedFlag("style", statuslineStyles)}},
				"vscode": {flags: []flagInfo{flag("template"), flag("dest"), flag("extensions-dir"),
					flag("keybindings-template"), flag("keybindings-dest"), boolFlag("check"), boolFlag("dry-run")}},
			}},
		"configure": {args: []func() []string{configureSections, providerNames}, flags: []flagInfo{
			valuedFlag("provider", providerNames), valuedFlag("agent", configureAgentNames), boolFlag("dry-run"), boolFlag("check"),
			boolFlag("reset"), boolFlag("restore"), flag("out"), boolFlag("vendors"), boolFlag("list"), formatFlag()}},
		"network": {sub: map[string]*compNode{
			"restart": {flags: []flagInfo{boolFlag("dry-run"), formatFlag()}},
			"tailscale": {flags: []flagInfo{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
				valuedFlag("exit-node", exitNodeActions), valuedFlag("route", routeTargets), flag("auth-key"),
				flag("key-env"), boolFlag("advertise-exit"),
				flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
				boolFlag("preauthorized"), flag("description")}},
			"browser":   {flags: []flagInfo{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames), boolFlag("dry-run")}},
			"websearch": {flags: []flagInfo{flag("query"), flag("max-results")}},
			"mcp":       {flags: []flagInfo{formatFlag()}},
			"page":      {flags: []flagInfo{flag("addr"), flag("hosts"), flag("days"), boolFlag("dry-run-dispatch")}},
			"view": {args: []func() []string{webViewAgentIDs}, flags: []flagInfo{flag("session"), flag("days"),
				flag("n"), boolFlag("follow"), formatFlag()}},
			"vscode": {flags: []flagInfo{flag("port"), boolFlag("stop"), boolFlag("status"), boolFlag("no-tailscale"),
				boolFlag("funnel"), flag("folder"), formatFlag()}},
		}},
		"models": {flags: []flagInfo{valuedFlag("provider", providerNames), boolFlag("unpin"), boolFlag("live"),
			valuedFlag("pin", modelPinSlots), boolFlag("dry-run"), formatFlag()}},
		"gateway": {flags: gatewayRouteFlags(), sub: map[string]*compNode{
			"route": {flags: gatewayRouteFlags()},
			"proxy": {flags: []flagInfo{boolFlag("start"), boolFlag("stop"), boolFlag("status"), boolFlag("serve"),
				flag("port"), formatFlag()}},
		}},
		"workflow": {sub: map[string]*compNode{
			"skeleton": {args: []func() []string{workflowPatterns}, flags: []flagInfo{flag("name")}},
			"lint":     {},
		}},
		"review": {sub: reviewSubNodes()},
		"vault": {sub: map[string]*compNode{
			"list": {args: []func() []string{vaultSections}, flags: []flagInfo{boolFlag("tailscale"), formatFlag()}},
			"set": {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("tailscale"), boolFlag("api-key"),
				boolFlag("dry-run")}},
			"get":           {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("export"), boolFlag("key")}},
			"delete":        {args: []func() []string{secretNames}},
			"import":        {flags: []flagInfo{boolFlag("yes")}},
			"export":        {args: []func() []string{secretNames}, flags: []flagInfo{flag("out")}},
			"edit":          {},
			"rotate-secret": {flags: []flagInfo{boolFlag("force")}},
		}},
		"databricks": {sub: map[string]*compNode{
			"check": {flags: []flagInfo{boolFlag("all"), flag("env"), boolFlag("1m"), flag("model")}},
			"login": {args: []func() []string{providerNames}, flags: []flagInfo{flag("auth-type"), boolFlag("dry-run")}},
			"import": {flags: []flagInfo{flag("file"), flag("profile"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
				boolFlag("seal-secrets"), boolFlag("apply")}},
			"export": {flags: []flagInfo{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
			"config": {args: []func() []string{func() []string { return []string{"claude", "codex"} }}, flags: dbxCfgFlags(true)},
			"rescue": {flags: append(dbxCfgFlags(false), boolFlag("print"))},
			"agents": {args: []func() []string{func() []string { return []string{"on", "off", "status"} }},
				flags: []flagInfo{flag("env")}},
		}},
		"history": {sub: map[string]*compNode{
			"snapshot": {flags: []flagInfo{flag("note")}},
			"log":      {flags: []flagInfo{flag("n"), formatFlag()}},
			"diff":     {},
		}},
		"agents": {sub: map[string]*compNode{
			"sync": {flags: []flagInfo{valuedFlag("vendor", vendorNames), boolFlag("dry-run"), boolFlag("check"), boolFlag("no-prune")}},
			"tree": {flags: []flagInfo{flag("days"), flag("session"), boolFlag("running"), boolFlag("all"),
				boolFlag("watch"), flag("interval"), formatFlag()}},
			"reap": {flags: []flagInfo{boolFlag("dry-run")}},
		}},
		"install": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("verbose"), valuedFlag("tools", toolNames), valuedFlag("setup", setupModes)}, sub: map[string]*compNode{"--setup": {sub: map[string]*compNode{
			"adopt":     {flags: []flagInfo{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
			"nuke":      {flags: []flagInfo{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
			"workspace": {args: []func() []string{setupWorkspaceSubwords}, flags: []flagInfo{flag("with"), boolFlag("dry-run")}},
		}}}},
		"dispatch": {flags: []flagInfo{valuedFlag("host", dispatchHostNames), flag("dir"), valuedFlag("agent", dispatchAgentNames),
			flag("name"), boolFlag("dry-run"), boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
		"worktree": {sub: map[string]*compNode{
			"list":   {flags: []flagInfo{formatFlag()}},
			"create": {flags: []flagInfo{flag("branch")}},
			"merge": {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run"), flag("target"),
				boolFlag("no-clean"), valuedFlag("resolve-agent", worktreeResolveAgents), boolFlag("resume"), flag("verify"), boolFlag("no-verify")}},
			"clean":     {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run"), boolFlag("force")}},
			"clean-all": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("yes")}},
		}},
		"test": {sub: map[string]*compNode{"mutate": {flags: []flagInfo{flag("run")}}}},
		"memory": {sub: map[string]*compNode{
			"recall": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"),
				flag("load"), formatFlag()}},
			"remember": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), valuedFlag("type", memoryRememberTypes)}},
			"init":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"review": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("stale-days"), boolFlag("notice"),
				boolFlag("brief")}},
			"promote": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames),
				valuedFlag("to", memoryPromoteTargets), boolFlag("dry-run")}},
			"update":   {flags: []flagInfo{flag("transcript"), flag("cwd"), flag("session")}},
			"dream":    {flags: []flagInfo{flag("cwd"), boolFlag("force")}},
			"search":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"), formatFlag()}},
			"show":     {},
			"digest":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"index":    {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"path":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"fmt":      {flags: []flagInfo{boolFlag("check")}},
			"watch":    {flags: []flagInfo{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
			"timeline": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("n")}},
			"archive": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
				boolFlag("apply"), boolFlag("restore")}},
			"alias": {flags: []flagInfo{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
		}},
	}

	// Launchers that only ever get a bare node (no flags of their own — see noComplete).
	for _, v := range []string{"opus", "sonnet", "haiku", "fable", "science"} {
		tree[v] = &compNode{}
	}
	tree["anthropic"] = &compNode{flags: []flagInfo{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"), boolFlag("router"),
		boolFlag("print-env"), flag("keep-env")}}
	// Vendor agents: ask/resume/review/transfer/status/result/cancel/setup/models, same shape per vendor.
	for _, v := range []string{"codex", "copilot", "opencode"} {
		tree[v] = &compNode{sub: vendorSubNodes(askFlags)}
	}
	geminiSub := vendorSubNodes(askFlags)
	geminiSub["bridge"] = &compNode{flags: []flagInfo{flag("task"), flag("model"), valuedFlag("provider", providerNames), flag("effort"),
		flag("dirs"), flag("files"), flag("format"), flag("max-files"), flag("max-file-bytes"),
		flag("timeout"), boolFlag("print-command"), flag("rubric"), boolFlag("index"), boolFlag("diff")}}
	tree["gemini"] = &compNode{sub: geminiSub}
	// Every remaining verb (hook/statusline/version/completion/__complete and anything future) at
	// least gets an empty node so it's never silently unrepresented.
	for _, c := range commands() {
		if tree[c.name] == nil {
			tree[c.name] = &compNode{}
		}
	}
	tree["hook"] = &compNode{args: []func() []string{hookNames}}
	return tree
}

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []flagInfo {
	fs := []flagInfo{{name: "env"}, {name: "provider", values: providerNames}, {name: "host"}, {name: "wire"},
		{name: "model"}, {name: "auth"}, {name: "set"}, {name: "unset"}}
	if withOut {
		fs = append(fs, flagInfo{name: "out"}, flagInfo{name: "force", isBool: true})
	}
	return fs
}

// vendorSubNodes is the one builder for every vendor agent's completion subtree (codex, copilot,
// opencode, gemini): ask/resume/review/transfer/status/result/cancel/setup/models, same shape per vendor.
func vendorSubNodes(askFlags []flagInfo) map[string]*compNode {
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
		"env":      {args: []func() []string{func() []string { return providers.DatabricksEnvs }}},
		"setup":    {flags: []flagInfo{formatFlag()}},
		"models":   {},
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

func worktreeResolveAgents() []string { return []string{"claude", "codex"} }

// memoryRememberTypes mirrors memory.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return []string{"user", "feedback", "project", "reference"} }

// memoryPromoteTargets mirrors memory.PromoteTargets for `memory promote --to` completion.
func memoryPromoteTargets() []string { return memory.PromoteTargets }

// exitNodeActions are the `network tailscale --exit-node` values.
func exitNodeActions() []string { return []string{"on", "off", "approve"} }

// routeTargets are the `network tailscale --route` values: dispatch hosts or "off" to clear.
func routeTargets() []string { return append(dispatchHostNames(), "off") }

// statuslineStyles are the `theme statusline --style` values.
func statuslineStyles() []string {
	return []string{"gradient", "powerline", "capsule", "minimal", "dashboard"}
}
