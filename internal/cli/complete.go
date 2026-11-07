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

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/ui/helptext"
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

// parseCompletionArgs reports --install, or done with the exit code for -h and for an unknown word.
func parseCompletionArgs(args []string) (install bool, code int, done bool) {
	for _, a := range args {
		switch a {
		case "bash": // the only shell; accepted for the conventional `completion bash` spelling
		case "--install":
			install = true
		case "-h", "--help", "help":
			completionHelp()
			return false, 0, true
		default:
			fmt.Fprintf(os.Stderr, "completion: unknown argument %q (try -h)\n", a)
			return false, 2, true
		}
	}
	return install, 0, false
}

func completionHelp() {
	fs := helptext.NewFlagSet("completion", completionPage)
	fs.SetOutput(os.Stdout)
	fs.Bool("install", false, "install bash completion for new shells")
	fs.Usage()
}

// cmdCompletion prints the bash completion script (default) or, with --install, writes it to the
// bash-completion user dir (internal/completion.Install).
func cmdCompletion(args []string) int {
	install, code, done := parseCompletionArgs(args)
	if done {
		return code
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

// formatValues is the shared text|json domain almost every --format flag offers.
func formatValues() []string { return []string{helptext.Text, helptext.JSON} }
