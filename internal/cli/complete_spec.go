package cli

import (
	"sync"

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

	askFlags := []flagInfo{
		valuedFlag("agent", agentNames), valuedFlag("provider", providerNames), flag("model"),
		flag("effort"), flag("workdir"), flag("session"), boolFlag("tools"), boolFlag("native"),
		flag("timeout"), flag("format"), flag("rubric"), flag("retry"), boolFlag("log"),
	}

	tree := map[string]*compNode{
		"resolve":  {flags: []flagInfo{valuedFlag("provider", providerNames), boolFlag("router")}},
		"switch":   {args: []func() []string{providerNames}, flags: []flagInfo{boolFlag("dry-run")}},
		"profile":  {args: []func() []string{providerNames}},
		"clean":    {flags: []flagInfo{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}},
		"generate": {flags: []flagInfo{flag("out")}},
		"usage":    {flags: []flagInfo{flag("session"), flag("days"), formatFlag(), boolFlag("workflows"), boolFlag("providers")}},
		"describe": {flags: []flagInfo{formatFlag()}},
		"doctor":   {flags: []flagInfo{boolFlag("audit"), flag("days"), formatFlag()}},
		"vscode": {flags: []flagInfo{flag("template"), flag("dest"), flag("extensions-dir"),
			flag("keybindings-template"), flag("keybindings-dest"), boolFlag("check"), boolFlag("dry-run")}},
		"help-svg": {flags: []flagInfo{flag("out"), flag("check")}},
		"run":      {args: []func() []string{vendorNames}},
		"ask":      {flags: askFlags},
		"theme": {args: []func() []string{statusline.GradientNames}, flags: []flagInfo{boolFlag("preview")},
			sub: map[string]*compNode{"apply": {flags: []flagInfo{boolFlag("dry-run")}}}},
		"configure": {args: []func() []string{configureSections}, flags: []flagInfo{
			valuedFlag("provider", providerNames), valuedFlag("agent", configureAgentNames), boolFlag("dry-run"), boolFlag("check")}},
		"models": {flags: []flagInfo{valuedFlag("provider", providerNames), boolFlag("unpin"), boolFlag("live"),
			valuedFlag("pin", modelPinSlots), boolFlag("dry-run"), formatFlag()}},
		"router": {sub: map[string]*compNode{
			"serve": {flags: []flagInfo{flag("port")}}, "start": {}, "stop": {}, "status": {}}},
		"mcp": {sub: map[string]*compNode{"doctor": {flags: []flagInfo{formatFlag()}}}},
		"workflow": {sub: map[string]*compNode{
			"skeleton": {args: []func() []string{workflowPatterns}, flags: []flagInfo{flag("name")}},
			"lint":     {},
		}},
		"review": {sub: reviewSubNodes()},
		"gemini": {sub: map[string]*compNode{
			"ask": {flags: askFlags},
			"bridge": {flags: []flagInfo{flag("task"), flag("model"), valuedFlag("provider", providerNames), flag("effort"),
				flag("dirs"), flag("files"), flag("format"), flag("max-files"), flag("max-file-bytes"),
				flag("timeout"), boolFlag("print-command"), flag("rubric"), boolFlag("index"), boolFlag("diff")}},
		}},
		"secrets": {sub: map[string]*compNode{
			"set":    {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("dry-run")}},
			"get":    {args: []func() []string{secretNames}, flags: []flagInfo{boolFlag("export")}},
			"delete": {args: []func() []string{secretNames}},
			"import": {},
			"export": {args: []func() []string{secretNames}},
			"list":   {args: []func() []string{secretNames}},
			"status": {}, "scan": {},
			"git":    {flags: []flagInfo{flag("since")}},
			"doctor": {},
			"all":    {flags: []flagInfo{flag("since")}}, // forwards its args to `secrets git`
		}},
		"vault": {sub: map[string]*compNode{
			"show": {}, "edit": {}, "rollback": {}, "rotate-key": {},
			"migrate":  {flags: []flagInfo{boolFlag("dry-run")}},
			"finalize": {flags: []flagInfo{valuedFlag("discard", secretNames)}},
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
		}},
		"tailscale": {sub: map[string]*compNode{
			"status":   {flags: []flagInfo{formatFlag()}},
			"machines": {flags: []flagInfo{formatFlag()}},
			"connect":  {flags: []flagInfo{flag("auth-key"), flag("key-env"), boolFlag("exit-node")}},
			"key": {flags: []flagInfo{boolFlag("ephemeral"), boolFlag("preauthorized"), boolFlag("reusable"),
				flag("tags"), flag("expiry"), flag("description")}},
			"exit-node": {flags: []flagInfo{boolFlag("enable"), boolFlag("disable"), boolFlag("approve")}},
		}},
		"install": {flags: []flagInfo{boolFlag("dry-run"), valuedFlag("tools", toolNames), valuedFlag("setup", setupModes)}, sub: map[string]*compNode{"--setup": {sub: map[string]*compNode{
			"adopt":     {flags: []flagInfo{flag("from"), flag("into"), boolFlag("dry-run"), flag("label")}},
			"nuke":      {flags: []flagInfo{flag("dir"), boolFlag("apply"), boolFlag("keep-memory"), boolFlag("account"), boolFlag("cli")}},
			"workspace": {args: []func() []string{setupWorkspaceSubwords}, flags: []flagInfo{flag("with"), boolFlag("dry-run")}},
		}}}},
		"dispatch": {flags: []flagInfo{valuedFlag("host", dispatchHostNames), flag("dir"), valuedFlag("agent", dispatchAgentNames),
			flag("name"), boolFlag("dry-run"), boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
		"web": {flags: []flagInfo{flag("addr"), flag("hosts"), flag("days"), boolFlag("dry-run-dispatch")}, sub: map[string]*compNode{
			"browsers": {flags: []flagInfo{formatFlag()}},
			"setup":    {flags: []flagInfo{valuedFlag("browser", browserNames), boolFlag("dry-run"), formatFlag()}},
			"view": {args: []func() []string{webViewAgentIDs}, flags: []flagInfo{flag("session"), flag("days"),
				flag("n"), boolFlag("follow"), formatFlag()}},
			"search": {flags: []flagInfo{flag("query"), flag("max-results")}},
		}},
		"worktree": {sub: map[string]*compNode{
			"list":   {flags: []flagInfo{formatFlag()}},
			"create": {flags: []flagInfo{flag("branch")}},
			"merge": {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run"), flag("target"),
				boolFlag("no-clean"), valuedFlag("resolve-agent", worktreeResolveAgents), boolFlag("resume"), flag("verify"), boolFlag("no-verify")}},
			"clean":     {args: []func() []string{worktreeNames}, flags: []flagInfo{boolFlag("dry-run")}},
			"clean-all": {flags: []flagInfo{boolFlag("dry-run"), boolFlag("yes")}},
		}},
		"memory": {sub: map[string]*compNode{
			"init":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"search":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"), formatFlag()}},
			"show":     {},
			"digest":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"index":    {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"path":     {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"update":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("transcript"), flag("cwd"), flag("session")}},
			"dream":    {flags: []flagInfo{flag("cwd"), boolFlag("force")}},
			"watch":    {flags: []flagInfo{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
			"doctor":   {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("stale-days"), boolFlag("notice")}},
			"stats":    {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"timeline": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("n")}},
			"promote":  {flags: []flagInfo{valuedFlag("repo", memoryRepoNames)}},
			"archive": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
				boolFlag("apply"), boolFlag("restore")}},
			"alias":   {flags: []flagInfo{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
			"migrate": {flags: []flagInfo{boolFlag("dry-run"), flag("manifest"), boolFlag("supersede"), flag("origin"), formatFlag()}},
		}},
	}

	// Launchers that only ever get a bare node (no flags of their own — see noComplete).
	for _, v := range []string{"opus", "sonnet", "haiku", "fable", "science", "copilot", "codex", "opencode"} {
		tree[v] = &compNode{}
	}
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
