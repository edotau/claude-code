package completion

import (
	"fmt"
	"slices"
	"strings"
)

// Flag describes a completable flag without its leading dashes.
type Flag struct {
	Name   string
	IsBool bool
	Values func() []string
}

// Node describes a verb or subcommand and its completion domains.
type Node struct {
	Sub   map[string]*Node  // Subcommand nodes keyed by literal word.
	Flags []Flag            // This node's own flags.
	Args  []func() []string // Positional domains in order; nil entries have no domain.
	Rest  func() []string   // Domain beyond Args; nil means free text.
}

// Verb is the command metadata used for top-level completion.
type Verb struct {
	Name   string
	Hidden bool
}

// Engine completes words using command metadata without dispatching commands.
type Engine struct {
	Tree    func() map[string]*Node
	Resolve func(string) (string, bool)
	Verbs   func() []Verb
}

// noComplete suppresses native launcher flags only before an in-process subword is selected.
var noComplete = map[string]bool{
	"opus": true, "sonnet": true, "haiku": true, "fable": true, "codex": true, "copilot": true, "opencode": true, "gemini": true,
	"science": true, "claude": true,
}

// knownPath separates the canonical command path from already-typed flags, values and positionals.
func (e Engine) knownPath(words []string) (path []string, node *Node, rest []string) {
	if len(words) == 0 {
		return nil, nil, words
	}
	name, ok := e.Resolve(words[0])
	if !ok {
		return nil, nil, words
	}
	n := e.Tree()[name]
	if n == nil {
		n = &Node{}
	}
	path = []string{name}
	i := 1
	for i < len(words) && n.Sub != nil {
		next, ok := n.Sub[words[i]]
		if !ok { // A sub keyed "--setup" is the only one a flag word can enter.
			break
		}
		n, path = next, append(path, words[i])
		i++
	}
	return path, n, words[i:]
}

// flagLookup resolves node-local flags, excluding flags owned by a bare launcher.
func flagLookup(path []string, node *Node, flagWord string) (Flag, bool) {
	if len(path) == 1 && noComplete[path[0]] {
		return Flag{}, false
	}
	name := strings.TrimLeft(flagWord, "-")
	if node == nil {
		return Flag{}, false
	}
	for _, f := range node.Flags {
		if f.Name == name {
			return f, true
		}
	}
	return Flag{}, false
}

func flagIsBool(path []string, node *Node, flagWord string) bool {
	f, ok := flagLookup(path, node, flagWord)
	return ok && f.IsBool
}

// flagValues returns nil for boolean, unknown or domainless flags.
func flagValues(path []string, node *Node, flagWord string) []string {
	f, ok := flagLookup(path, node, flagWord)
	if !ok || f.IsBool || f.Values == nil {
		return nil
	}
	return f.Values()
}

// remainingFlags sorts node-local flags and removes already-typed flags.
func remainingFlags(path []string, node *Node, rest []string) []string {
	if len(path) == 1 && noComplete[path[0]] {
		return nil
	}
	if node == nil {
		return nil
	}
	var out []string
	for _, f := range node.Flags {
		out = append(out, "--"+f.Name)
	}
	slices.Sort(out)
	return slices.DeleteFunc(out, func(f string) bool { return slices.Contains(rest, f) })
}

// positionalInfo counts satisfied positional slots, skipping values consumed by non-boolean flags.
func positionalInfo(path []string, node *Node, rest []string) (count int) {
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		if !strings.HasPrefix(w, "-") {
			count++
			continue
		}
		if strings.Contains(w, "=") { // --flag=value consumes no following word.
			continue
		}
		if !flagIsBool(path, node, w) && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
			i++ // The next word is this flag's value, not a positional.
		}
	}
	return count
}

// positionalDomain uses Rest for slots beyond Args; nil leaves completion to the shell.
func positionalDomain(node *Node, i int) func() []string {
	if node == nil {
		return nil
	}
	if i < len(node.Args) {
		return node.Args[i]
	}
	return node.Rest
}

// topLevelCandidates includes hidden commands only once a nonempty prefix has narrowed intent.
func (e Engine) topLevelCandidates(cur string) []string {
	var cands []string
	for _, c := range e.Verbs() {
		if c.Hidden {
			if cur != "" {
				cands = append(cands, c.Name)
			}
			continue
		}
		cands = append(cands, c.Name)
	}
	return append(cands, "help")
}

// Candidates completes shell words after the binary name; the last word is the partial word.
func (e Engine) Candidates(words []string) []string {
	cur := ""
	if len(words) > 0 {
		cur = words[len(words)-1]
		words = words[:len(words)-1]
	}
	path, node, rest := e.knownPath(words)
	if strings.HasPrefix(cur, "-") && strings.Contains(cur, "=") {
		return inlineFlagCandidates(path, node, cur)
	}
	var out []string
	for _, c := range e.nextCompletionCandidates(path, node, rest, cur) {
		if strings.HasPrefix(c, cur) {
			out = append(out, c)
		}
	}
	return out
}

func (e Engine) nextCompletionCandidates(path []string, node *Node, rest []string, cur string) []string {
	switch {
	case len(rest) > 0 && strings.HasPrefix(rest[len(rest)-1], "-") && !strings.HasPrefix(cur, "-") &&
		!flagIsBool(path, node, rest[len(rest)-1]):
		return flagValues(path, node, rest[len(rest)-1])
	case len(path) == 0 && len(rest) == 0:
		return e.topLevelCandidates(cur)
	case strings.HasPrefix(cur, "-"):
		return remainingFlags(path, node, rest)
	default:
		return positionAwareCandidates(path, node, rest, cur)
	}
}

func inlineFlagCandidates(path []string, node *Node, cur string) []string {
	name, valPrefix, _ := strings.Cut(cur, "=")
	var out []string
	for _, v := range flagValues(path, node, name) {
		if strings.HasPrefix(v, valPrefix) {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// positionAwareCandidates combines subwords, positional domains and applicable remaining flags.
func positionAwareCandidates(path []string, node *Node, rest []string, cur string) []string {
	i := positionalInfo(path, node, rest)
	var cands []string
	if i == 0 && node != nil {
		subs := make([]string, 0, len(node.Sub))
		for w := range node.Sub {
			subs = append(subs, w)
		}
		slices.Sort(subs)
		cands = append(cands, subs...)
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

// Complete prints one candidate per line for the hidden shell-completion verb.
func (e Engine) Complete(args []string) int {
	for _, c := range e.Candidates(args) {
		fmt.Println(c)
	}
	return 0
}
