// Bash tab-completion. Every verb/subverb/flag/positional lives in one spec tree (buildSpecTree,
// with its value-domain functions below), keyed from commands() so every verb is
// represented — a launcher verb (opus/sonnet/haiku/fable/codex/copilot/opencode/gemini/science, see
// noComplete) still offers nothing beyond its own subwords, since nothing here ever calls a run func.
package cli

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/menu"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/worktree"
)

// completionBin is the installed binary name the completion script registers against.
const completionBin = "claude-code"

// flagInfo is one completable flag: name (no leading --), whether it takes no value (bool), and an
// optional source for the candidates offered for the word that follows it (or after "=").
type flagInfo struct {
	name   string
	isBool bool
	values func() []string
}

// compNode is one point in the completion spec tree: a verb or one of its subcommand words.
type compNode struct {
	sub   map[string]*compNode // subcommand words, keyed by the literal word
	flags []flagInfo           // this node's own flags
	args  []func() []string    // positional slot domains, in order; a nil entry means no domain
	rest  func() []string      // domain for positions beyond len(args); nil = free text (no completion)
}

// noComplete names the verbs that forward argv to a launched agent CLI (or, for science, exec a
// child with argv): a bare "<verb> --<TAB>" must offer nothing, since their flags belong to a
// process this binary never inspects. A chosen subword (gemini ask) is unaffected —
// noComplete only gates flag lookups at depth 1 (see flagLookup/remainingFlags).
var noComplete = map[string]bool{
	"opus": true, "sonnet": true, "haiku": true, "fable": true, "codex": true, "copilot": true, "opencode": true, "gemini": true,
	"science": true, "claude": true,
}

// knownPath splits words into the known node path (canonical verb name, plus any resolved subwords)
// and the rest (flags/values/positionals already typed). nil path ⇒ the first word is not a command.
func knownPath(words []string) (path []string, node *compNode, rest []string) {
	if len(words) == 0 {
		return nil, nil, words
	}
	cmd := lookup(words[0])
	if cmd == nil {
		return nil, nil, words
	}
	tree := specTree()
	n := tree[cmd.name]
	if n == nil {
		n = &compNode{}
	}
	path = []string{cmd.name} // canonical: an alias (use/token) resolves to switch/resolve here
	i := 1
	for i < len(words) && n.sub != nil {
		next, ok := n.sub[words[i]]
		if !ok { // a sub keyed "--setup" is the only one a flag word can enter
			break
		}
		n, path = next, append(path, words[i])
		i++
	}
	return path, n, words[i:]
}

// flagLookup resolves a typed "--name" against node's own flags; false when node is a bare launcher
// (no subword chosen yet) or the flag is unknown.
func flagLookup(path []string, node *compNode, flagWord string) (flagInfo, bool) {
	if len(path) == 1 && noComplete[path[0]] {
		return flagInfo{}, false
	}
	name := strings.TrimLeft(flagWord, "-")
	if node == nil {
		return flagInfo{}, false
	}
	for _, f := range node.flags {
		if f.name == name {
			return f, true
		}
	}
	return flagInfo{}, false
}

func flagIsBool(path []string, node *compNode, flagWord string) bool {
	f, ok := flagLookup(path, node, flagWord)
	return ok && f.isBool
}

// flagValues completes the word after flagWord: nil when it's boolean, unknown, or has no domain.
func flagValues(path []string, node *compNode, flagWord string) []string {
	f, ok := flagLookup(path, node, flagWord)
	if !ok || f.isBool || f.values == nil {
		return nil
	}
	return f.values()
}

// remainingFlags lists node's own flags minus the ones already typed (rest); nil for a bare launcher.
func remainingFlags(path []string, node *compNode, rest []string) []string {
	if len(path) == 1 && noComplete[path[0]] {
		return nil
	}
	if node == nil {
		return nil
	}
	var out []string
	for _, f := range node.flags {
		out = append(out, "--"+f.name)
	}
	slices.Sort(out)
	return slices.DeleteFunc(out, func(f string) bool { return slices.Contains(rest, f) })
}

// positionalInfo walks rest (the words after the resolved node, all already typed) and reports how
// many positional slots they satisfied, skipping values consumed by a non-bool flag.
func positionalInfo(path []string, node *compNode, rest []string) (count int) {
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		if !strings.HasPrefix(w, "-") {
			count++
			continue
		}
		if strings.Contains(w, "=") { // --flag=value consumes no following word
			continue
		}
		if !flagIsBool(path, node, w) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
			i++ // the next word is this flag's value, not a positional
		}
	}
	return count
}

// positionalDomain returns the domain for positional slot i (the len(args)'th-and-beyond slots use rest).
func positionalDomain(node *compNode, i int) func() []string {
	if node == nil {
		return nil
	}
	if i < len(node.args) {
		return node.args[i]
	}
	return node.rest
}

// topLevelCandidates lists every visible command (+ "help"); a hidden command only
// joins the list once a prefix has narrowed intent, so the bare double-Tab menu stays canonical.
func topLevelCandidates(cur string) []string {
	var cands []string
	for _, c := range commands() {
		if c.hidden {
			if cur != "" {
				cands = append(cands, c.name)
			}
			continue
		}
		cands = append(cands, c.name)
	}
	return append(cands, "help")
}

// completionCandidates returns tab-completion words for the shell words typed after the program
// name (last = the partial word being completed).
func completionCandidates(words []string) []string {
	cur := ""
	if len(words) > 0 {
		cur = words[len(words)-1]
		words = words[:len(words)-1]
	}
	path, node, rest := knownPath(words)

	// "--flag=value": the current word itself carries the flag name before "=".
	if strings.HasPrefix(cur, "-") && strings.Contains(cur, "=") {
		return inlineFlagCandidates(path, node, cur)
	}

	var out []string
	for _, c := range nextCompletionCandidates(path, node, rest, cur) {
		if strings.HasPrefix(c, cur) {
			out = append(out, c)
		}
	}
	return out
}

func nextCompletionCandidates(path []string, node *compNode, rest []string, cur string) []string {
	switch {
	case len(rest) > 0 && strings.HasPrefix(rest[len(rest)-1], "-") && !strings.HasPrefix(cur, "-") &&
		!flagIsBool(path, node, rest[len(rest)-1]):
		return flagValues(path, node, rest[len(rest)-1]) // the value of the flag just typed
	case len(path) == 0 && len(rest) == 0:
		return topLevelCandidates(cur)
	case strings.HasPrefix(cur, "-"):
		return remainingFlags(path, node, rest)
	default:
		return positionAwareCandidates(path, node, rest, cur)
	}
}

func inlineFlagCandidates(path []string, node *compNode, cur string) []string {
	name, valPrefix, _ := strings.Cut(cur, "=")
	var out []string
	for _, v := range flagValues(path, node, name) {
		if strings.HasPrefix(v, valPrefix) {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// positionAwareCandidates handles the "next bare word" position: subwords (only at slot 0, right
// after the verb path), the positional domain for that slot (or rest beyond it), and — whenever
// that slot has no domain, or it's already satisfied and cur is empty — the node's remaining flags.
func positionAwareCandidates(path []string, node *compNode, rest []string, cur string) []string {
	i := positionalInfo(path, node, rest)
	var cands []string
	if i == 0 && node != nil {
		for w := range node.sub {
			cands = append(cands, w)
		}
	}
	domain := positionalDomain(node, i)
	if domain != nil {
		cands = append(cands, domain()...)
	}
	if domain == nil || (cur == "" && i > 0) {
		cands = append(cands, remainingFlags(path, node, rest)...)
	}
	return cands
}

// cmdComplete is the hidden __complete verb behind the shell shim: it receives the words typed after
// the program name and prints one candidate per line (empty output → shell default: filenames).
func cmdComplete(args []string) int {
	for _, c := range completionCandidates(args) {
		fmt.Println(c)
	}
	return 0
}

// completionPage: bare prints the script; zsh can source it after bashcompinit.
var completionPage = helptext.Page{
	Usage: "claude-code completion [bash] [flags]",
	About: "Print the bash tab-completion script (eval or source it). zsh: run " +
		"'autoload -U bashcompinit && bashcompinit', then source it.",
}

// cmdCompletion prints the bash completion script (default) or, with --install, writes it to the
// bash-completion user dir (internal/completion.Install).
func cmdCompletion(args []string) int {
	install := false
	for _, a := range args {
		switch a {
		case "bash": // the only shell; accepted for the conventional `completion bash` spelling
		case "--install":
			install = true
		case "-h", "--help", "help":
			fs := helptext.NewFlagSet("completion", completionPage)
			fs.SetOutput(os.Stdout)
			fs.Bool("install", false, "install bash completion for new shells")
			fs.Usage()
			return 0
		default:
			fmt.Fprintf(os.Stderr, "completion: unknown argument %q (try -h)\n", a)
			return 2
		}
	}
	if !install {
		fmt.Print(completion.Script(completionBin))
		return 0
	}
	path, err := completion.Install(completionBin)
	if err != nil {
		return helptext.Fail("completion: %v", err)
	}
	fmt.Println(path)
	fmt.Println("installed — open a new shell (or 'source' the file) and double-Tab claude-code")
	return 0
}

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
	askFlags := askSpecFlags()
	tree := coreSpecNodes()
	for _, nodes := range []map[string]*compNode{networkSpecNodes(), integrationSpecNodes(), workspaceSpecNodes(), memorySpecNodes()} {
		maps.Copy(tree, nodes)
	}
	maps.Copy(tree["agents"].sub, agentsRunSpecNodes(askFlags))
	addLauncherSpecNodes(tree, askFlags)
	return tree
}

func specFlag(name string) flagInfo     { return flagInfo{name: name} }
func specBoolFlag(name string) flagInfo { return flagInfo{name: name, isBool: true} }
func specValuedFlag(name string, values func() []string) flagInfo {
	return flagInfo{name: name, values: values}
}
func specFormatFlag() flagInfo { return specValuedFlag("format", formatValues) }

// gateway router's flags, which bare `gateway` also takes.
func gatewayRouteFlags() []flagInfo {
	flag, valuedFlag, formatFlag := specFlag, specValuedFlag, specFormatFlag
	return []flagInfo{valuedFlag("model", func() []string { return router.Aliases }),
		valuedFlag("agent", func() []string { return router.TableAgents }),
		flag("task"), valuedFlag("fallback", func() []string { return router.FallbackModes }), formatFlag()}
}

func vaultSections() []string {
	return []string{"secrets", "pins", "env", "providers", "agents", "settings", "vault", "all"}
}

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

func coreSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"resolve":  {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("proxy")}},
		"switch":   {args: []func() []string{providerNames}, flags: []flagInfo{boolFlag("dry-run")}},
		"clean":    {flags: []flagInfo{flag("target"), flag("older-than"), boolFlag("dry-run"), boolFlag("yes"), boolFlag("list")}},
		"usage":    {flags: []flagInfo{flag("session"), flag("days"), formatFlag(), boolFlag("workflows"), boolFlag("providers")}},
		"describe": {flags: []flagInfo{formatFlag()}},
		"doctor":   {flags: []flagInfo{boolFlag("audit"), flag("days"), boolFlag("scan"), boolFlag("git"), flag("since"), flag("range"), boolFlag("secrets"), formatFlag()}},
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
			boolFlag("restore"), boolFlag("revert"), flag("out"), boolFlag("agents"), boolFlag("list"), formatFlag()}},
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

func integrationSpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
		"models": {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("unpin"), boolFlag("live"),
			valuedFlag("pin", modelPinAliases), boolFlag("dry-run"), formatFlag()}},
		"gateway": {flags: gatewayRouteFlags(), sub: map[string]*compNode{
			"token":  {},
			"router": {flags: gatewayRouteFlags()},
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
			"check": {flags: []flagInfo{boolFlag("all"), valuedFlag("profile", providerNames), boolFlag("1m"), flag("model")}},
			"login": {args: []func() []string{providerNames}, flags: []flagInfo{flag("auth-type"), boolFlag("dry-run"), boolFlag("force"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"mint":  {flags: []flagInfo{valuedFlag("profile", providerNames), boolFlag("dry-run"), boolFlag("copy"), valuedFlag("as", func() []string { return []string{"user", "sp"} })}},
			"import": {flags: []flagInfo{flag("dbxcfg"), boolFlag("pick"), flag("rename"), boolFlag("replace"),
				boolFlag("seal-secrets"), boolFlag("apply")}},
			"export": {flags: []flagInfo{boolFlag("pick"), flag("rename"), flag("out"), boolFlag("merge"), boolFlag("force")}},
			"config": {args: []func() []string{func() []string { return []string{"claude", "codex"} }}, flags: dbxCfgFlags(true)},
			"rescue": {flags: append(dbxCfgFlags(false), boolFlag("print"))},
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

func memorySpecNodes() map[string]*compNode {
	flag, boolFlag, valuedFlag, formatFlag := specFlag, specBoolFlag, specValuedFlag, specFormatFlag
	return map[string]*compNode{
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
			"catchup":  {},
			"watch":    {flags: []flagInfo{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
			"timeline": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), flag("n")}},
			"archive": {flags: []flagInfo{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
				boolFlag("apply"), boolFlag("restore")}},
			"alias": {flags: []flagInfo{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
		}},
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

// dbxCfgFlags are the flags `databricks config` (withOut) and `databricks rescue` share.
func dbxCfgFlags(withOut bool) []flagInfo {
	fs := []flagInfo{{name: "profile", values: providerNames}, {name: "host"}, {name: "wire"},
		{name: "model"}, {name: "auth"}, {name: "set"}, {name: "unset"}}
	if withOut {
		fs = append(fs, flagInfo{name: "out"}, flagInfo{name: "force", isBool: true})
	}
	return fs
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

func worktreeResolveAgents() []string { return []string{"claude", "codex"} }

// memoryRememberTypes mirrors bank.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return []string{"user", "feedback", "project", "reference"} }

// memoryPromoteTargets mirrors memory.PromoteTargets for `memory promote --to` completion.
func memoryPromoteTargets() []string { return memory.PromoteTargets }

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
