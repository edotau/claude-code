// Bash tab-completion. Every verb/subverb/flag/positional lives in one spec tree (buildSpecTree,
// with its value-domain functions below), keyed from commands() so every verb is
// represented — a launcher verb still offers nothing beyond its own subwords, since the completion
// engine never calls a run func.
package cli

import (
	"fmt"
	"maps"
	"os"
	"sync"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// completionBin is the installed binary name the completion script registers against.
const completionBin = "claude-code"

func completionEngine() completion.Engine {
	return completion.Engine{
		Tree: specTree,
		Resolve: func(word string) (string, bool) {
			if c := lookup(word); c != nil {
				return c.name, true
			}
			return "", false
		},
		Verbs: func() []completion.Verb {
			var verbs []completion.Verb
			for _, c := range commands() {
				verbs = append(verbs, completion.Verb{Name: c.name, Hidden: c.hidden})
			}
			return verbs
		},
	}
}

func completionCandidates(words []string) []string {
	return completionEngine().Candidates(words)
}

// cmdComplete is the hidden completion verb behind the shell shim.
func cmdComplete(args []string) int {
	return completionEngine().Complete(args)
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
func specTree() map[string]*completion.Node {
	specTreeInit.Do(func() { specTreeCache = buildSpecTree() })
	return specTreeCache
}

var (
	specTreeInit  sync.Once
	specTreeCache map[string]*completion.Node
)

// buildSpecTree does the one-time work specTree caches. Every verb in commands() gets a node, even
// an empty one, so a launcher is never silently unrepresented. Subcommand words are hand data
// (mirroring each verb's own dispatch switch, same as before); the three former hand tables
// (subVerbs/positionals/flagsFor) are gone — this is the one place they're declared, nested instead
// of parallel.
func buildSpecTree() map[string]*completion.Node {
	askFlags := askSpecFlags()
	tree := coreSpecNodes()
	for _, nodes := range []map[string]*completion.Node{integrationSpecNodes(), workspaceSpecNodes()} {
		maps.Copy(tree, nodes)
	}
	for _, c := range commands() {
		if c.spec != nil {
			tree[c.name] = c.spec()
		}
	}
	maps.Copy(tree["agents"].Sub, agentsRunSpecNodes(askFlags))
	addLauncherSpecNodes(tree, askFlags)
	return tree
}

func specFlag(name string) completion.Flag     { return completion.Flag{Name: name} }
func specBoolFlag(name string) completion.Flag { return completion.Flag{Name: name, IsBool: true} }
func specValuedFlag(name string, values func() []string) completion.Flag {
	return completion.Flag{Name: name, Values: values}
}
func specFormatFlag() completion.Flag { return specValuedFlag("format", formatValues) }

// formatValues is the shared text|json domain almost every --format flag offers.
func formatValues() []string { return []string{helptext.Text, helptext.JSON} }
