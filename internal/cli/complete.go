// Bash tab-completion. Every verb/subverb/flag/positional lives in one spec tree (buildSpecTree in
// complete_spec.go; domain functions in complete_domains.go), keyed from commands() so every verb is
// represented — a launcher verb (opus/sonnet/haiku/fable/codex/copilot/opencode/gemini/run/science, see
// noComplete) still offers nothing beyond its own subwords, since nothing here ever calls a run func.
package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/launch"
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
// process this binary never inspects. A chosen subword (gemini ask, gemini bridge) is unaffected —
// noComplete only gates flag lookups at depth 1 (see flagLookup/remainingFlags).
var noComplete = map[string]bool{
	"opus": true, "sonnet": true, "haiku": true, "fable": true, "codex": true, "copilot": true, "opencode": true, "gemini": true,
	"run": true, "science": true,
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
		if !ok || strings.HasPrefix(words[i], "-") {
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
		if !flagIsBool(path, node, w) {
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

// topLevelCandidates lists every visible command (+ "help"); a hidden command (or an alias) only
// joins the list once a prefix has narrowed intent, so the bare double-Tab menu stays canonical.
func topLevelCandidates(cur string) []string {
	var cands []string
	for _, c := range commands() {
		if c.hidden {
			if cur != "" {
				cands = append(cands, c.name)
				cands = append(cands, c.aliases...)
			}
			continue
		}
		cands = append(cands, c.name)
		if cur != "" {
			cands = append(cands, c.aliases...)
		}
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
		name, valPrefix, _ := strings.Cut(cur, "=")
		var out []string
		for _, v := range flagValues(path, node, name) {
			if strings.HasPrefix(v, valPrefix) {
				out = append(out, name+"="+v)
			}
		}
		return out
	}

	var cands []string
	switch {
	case len(rest) > 0 && strings.HasPrefix(rest[len(rest)-1], "-") && !strings.HasPrefix(cur, "-") &&
		!flagIsBool(path, node, rest[len(rest)-1]):
		cands = flagValues(path, node, rest[len(rest)-1]) // the value of the flag just typed
	case len(path) == 0 && len(rest) == 0:
		cands = topLevelCandidates(cur)
	case strings.HasPrefix(cur, "-"):
		cands = remainingFlags(path, node, rest)
	default:
		cands = positionAwareCandidates(path, node, rest, cur)
	}
	var out []string
	for _, c := range cands {
		if strings.HasPrefix(c, cur) {
			out = append(out, c)
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

const completionUsage = "usage: claude-code completion [bash] [--install]"

// cmdCompletion prints the bash completion script (default) or, with --install, writes it to the
// bash-completion user dir (internal/completion.Install).
func cmdCompletion(args []string) int {
	install := false
	for _, a := range args {
		switch a {
		case "bash": // the only shell; accepted for the conventional `completion bash` spelling
		case "--install":
			install = true
		case "-h", "--help":
			fmt.Println(completionUsage)
			fmt.Println("  (no flag)   print the bash completion script (eval or source it)")
			fmt.Println("  --install   write it to the bash-completion user dir; new shells complete on Tab")
			fmt.Println("  zsh: run 'autoload -U bashcompinit && bashcompinit' first, then source the script")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "completion: unknown argument %q (try -h)\n", a)
			return 2
		}
	}
	if !install {
		fmt.Print(completion.Script(completionBin, launch.ShimNames))
		return 0
	}
	path, err := completion.Install(completionBin, launch.ShimNames)
	if err != nil {
		return fail("completion: %v", err)
	}
	fmt.Println(path)
	fmt.Println("installed — open a new shell (or 'source' the file) and double-Tab claude-code")
	return 0
}
