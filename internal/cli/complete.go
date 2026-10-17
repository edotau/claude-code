// Bash tab-completion, ported from claude-dbx's internal/cli/complete.go. This repo hand-dispatches
// every subcommand inside each verb's run func instead of building a registry Sub tree, and has no
// cliutil.OnFlagSet hook — so candidates come from static tables (subverbs, flags, value domains)
// kept beside the verbs they describe, never from probing a verb's own flagset. That also means a
// launcher verb (opus/sonnet/haiku/fable/codex/copilot/opencode/gemini/run/science — see noComplete) is never at risk
// of being dispatched: nothing here ever calls a command's run func.
package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/statusline"
)

// completionBin is the installed binary name the completion script registers against.
const completionBin = "claude-code"

// flagInfo is one completable flag: name (no leading --), whether it takes no value (bool), and an
// optional source for the candidates offered for the word that follows it.
type flagInfo struct {
	name   string
	isBool bool
	values func() []string
}

// subVerbs lists a verb's hand-dispatched subcommand words, keyed by the verb's canonical name (the
// command.name a command dispatches under — see knownPath). Only two levels deep: nothing in this
// repo hand-dispatches a third word.
var subVerbs = map[string][]string{
	"secrets":  {"set", "get", "status", "scan", "git", "doctor", "all"},
	"history":  {"snapshot", "log", "diff"},
	"memory":   {"init", "search", "index", "path", "update", "watch", "doctor", "stats", "timeline", "promote", "archive"},
	"router":   {"serve", "start", "stop", "status"},
	"mcp":      {"doctor"},
	"agents":   {"sync", "docs"},
	"review":   {"quality", "complexity", "assumptions", "goals", "diff", "pr", "report", "gate"},
	"workflow": {"skeleton", "lint"},
	"theme":    {"apply"},
	"gemini":   {"ask", "bridge"},
}

// noComplete names the verbs that forward argv to a launched agent CLI (or, for science, exec a
// child with argv): a bare "<verb> --<TAB>" must offer nothing, since their flags belong to a process
// this binary never inspects. A safe subword under one of these (gemini ask, gemini bridge) is
// unaffected — see the len(path)==1 guard in flagLookup/remainingFlags.
var noComplete = map[string]bool{
	"opus": true, "sonnet": true, "haiku": true, "fable": true, "codex": true, "copilot": true, "opencode": true, "gemini": true,
	"run": true, "science": true,
}

// flagsFor lists a path's own flags, keyed by the space-joined canonical path ("memory search",
// "ask"). Hand-maintained from each verb's flag.NewFlagSet/extract* calls — keep it in sync with the
// verb body it describes when either changes.
var flagsFor = map[string][]flagInfo{
	"resolve":          {{name: "provider", values: providerNames}, {name: "router", isBool: true}},
	"clean":            {{name: "what"}, {name: "older-than"}, {name: "apply", isBool: true}, {name: "list", isBool: true}},
	"secrets set":      {{name: "keyring", isBool: true}, {name: "dry-run", isBool: true}},
	"switch":           {{name: "dry-run", isBool: true}},
	"secrets get":      {{name: "export", isBool: true}},
	"secrets git":      {{name: "since"}},
	"secrets all":      {{name: "since"}}, // forwards its args to `secrets git`
	"history snapshot": {{name: "note"}},
	"history log":      {{name: "n"}, {name: "memory", isBool: true}, {name: "config", isBool: true}, {name: "format", values: func() []string { return []string{"text", "json"} }}},
	"history diff":     {{name: "memory", isBool: true}},
	"generate":         {{name: "out"}},
	"usage":            {{name: "session"}, {name: "days"}, {name: "format", values: func() []string { return []string{"text", "json"} }}, {name: "workflows", isBool: true}},
	"describe":         {{name: "format", values: func() []string { return []string{"text", "json"} }}},
	"audit":            {{name: "days"}, {name: "format", values: func() []string { return []string{"text", "json"} }}},
	"doctor":           {{name: "format", values: func() []string { return []string{"text", "json"} }}},
	"mcp doctor":       {{name: "format", values: func() []string { return []string{"text", "json"} }}},
	"install":          {{name: "dry-run", isBool: true}},
	"theme":            {{name: "preview", isBool: true}},
	"theme apply":      {{name: "dry-run", isBool: true}},
	// memory's flagset is built once per subcommand and always carries --repo (declared ahead of the
	// switch in cmdMemory), even on arms like stats/update that never read it back out.
	"memory init":     {{name: "repo"}},
	"memory search":   {{name: "repo"}, {name: "k"}, {name: "format", values: func() []string { return []string{"text", "json"} }}},
	"memory index":    {{name: "repo"}},
	"memory path":     {{name: "repo"}},
	"memory update":   {{name: "repo"}, {name: "transcript"}, {name: "cwd"}, {name: "session"}},
	"memory watch":    {{name: "session"}, {name: "transcript"}, {name: "cwd"}, {name: "owner"}, {name: "idle"}},
	"memory doctor":   {{name: "repo"}, {name: "stale-days"}, {name: "notice", isBool: true}},
	"memory stats":    {{name: "repo"}},
	"memory timeline": {{name: "repo"}, {name: "n"}},
	"memory promote":  {{name: "repo"}},
	"memory archive":  {{name: "repo"}, {name: "dupes", isBool: true}, {name: "split"}, {name: "apply", isBool: true}, {name: "restore", isBool: true}},
	"router serve":    {{name: "port"}},
	"ask": {{name: "agent", values: agentNames}, {name: "provider", values: providerNames}, {name: "model"},
		{name: "effort"}, {name: "workdir"}, {name: "session"}, {name: "tools", isBool: true},
		{name: "timeout"}, {name: "format"}, {name: "rubric"}, {name: "retry"}, {name: "log", isBool: true}},
	"agents sync": {{name: "vendor"}, {name: "dry-run", isBool: true}, {name: "check", isBool: true}, {name: "no-prune", isBool: true}},
	"agents docs": {{name: "check", isBool: true}, {name: "dry-run", isBool: true}},
	"models":      {{name: "provider", values: providerNames}, {name: "unpin", isBool: true}, {name: "live", isBool: true}, {name: "pin"}, {name: "dry-run", isBool: true}},
	"gemini bridge": {{name: "task"}, {name: "model"}, {name: "provider", values: providerNames}, {name: "effort"},
		{name: "dirs"}, {name: "files"}, {name: "format"}, {name: "max-files"}, {name: "max-file-bytes"},
		{name: "timeout"}, {name: "print-command", isBool: true}, {name: "rubric"}, {name: "index", isBool: true}, {name: "diff", isBool: true}},
	"workflow skeleton": {{name: "name"}},
}

// "gemini ask" is `ask` with --agent/--provider pre-filled; the flags it accepts are the same ones.
func init() { flagsFor["gemini ask"] = flagsFor["ask"] }

// positionals complete a path's first bare argument, keyed by the canonical path.
var positionals = map[string]func() []string{
	"switch":            providerNames,
	"profile":           providerNames,
	"theme":             statusline.GradientNames,
	"run":               vendorNames,
	"hook":              hookNames,
	"workflow skeleton": workflowPatterns,
}

func vendorNames() []string { return []string{"codex", "gemini", "opencode", "copilot"} }

func hookNames() []string {
	return []string{"safety", "stop-format", "context-checkpoint", "session-start",
		"memory-recall", "session-harvest", "session-harvest-end"}
}

func workflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// providerNames reads the provider registry straight off disk (no network): the same file
// `claude-code profile`/`switch` already read.
func providerNames() []string {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

// agentNames lists the `ask --agent` runner names; agent.Runners() only builds the roster, it never
// dials a runner.
func agentNames() []string {
	var out []string
	for _, r := range agent.Runners() {
		out = append(out, r.Name())
	}
	return out
}

// knownPath splits the typed words into the known verb path (canonical name, plus a subVerbs word)
// and the rest (flags/values/positionals). nil path ⇒ the first word is not a command.
func knownPath(words []string) (path, rest []string) {
	if len(words) == 0 {
		return nil, words
	}
	cmd := lookup(words[0])
	if cmd == nil {
		return nil, words
	}
	path = []string{cmd.name} // canonical: an alias (use/token) resolves to switch/resolve here
	if len(words) > 1 && isSubWord(cmd.name, words[1]) {
		path = append(path, words[1])
	}
	return path, words[len(path):]
}

func isSubWord(verb, w string) bool {
	return !strings.HasPrefix(w, "-") && slices.Contains(subVerbs[verb], w)
}

// flagLookup resolves a typed "--name" against path's own flags; false when path is a bare launcher
// (no subword chosen yet) or the flag is unknown.
func flagLookup(path []string, flagWord string) (flagInfo, bool) {
	if len(path) == 1 && noComplete[path[0]] {
		return flagInfo{}, false
	}
	name := strings.TrimLeft(flagWord, "-")
	for _, f := range flagsFor[strings.Join(path, " ")] {
		if f.name == name {
			return f, true
		}
	}
	return flagInfo{}, false
}

func flagIsBool(path []string, flagWord string) bool {
	f, ok := flagLookup(path, flagWord)
	return ok && f.isBool
}

// flagValues completes the word after flagWord: nil when it's boolean, unknown, or has no domain.
func flagValues(path []string, flagWord string) []string {
	f, ok := flagLookup(path, flagWord)
	if !ok || f.isBool || f.values == nil {
		return nil
	}
	return f.values()
}

// remainingFlags lists path's own flags minus the ones already typed (rest); nil for a bare launcher.
func remainingFlags(path, rest []string) []string {
	if len(path) == 1 && noComplete[path[0]] {
		return nil
	}
	var out []string
	for _, f := range flagsFor[strings.Join(path, " ")] {
		out = append(out, "--"+f.name)
	}
	slices.Sort(out)
	return slices.DeleteFunc(out, func(f string) bool { return slices.Contains(rest, f) })
}

func subWordsFor(path []string) []string {
	if len(path) != 1 {
		return nil
	}
	return subVerbs[path[0]]
}

func positionalsFor(path []string) func() []string {
	return positionals[strings.Join(path, " ")]
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
	path, rest := knownPath(words)
	var cands []string
	switch {
	case len(rest) > 0 && strings.HasPrefix(rest[len(rest)-1], "-") && !strings.HasPrefix(cur, "-") &&
		!flagIsBool(path, rest[len(rest)-1]):
		cands = flagValues(path, rest[len(rest)-1]) // the value of the flag just typed
	case len(rest) > 0 && strings.HasPrefix(rest[len(rest)-1], "-") && !strings.HasPrefix(cur, "-"):
		// the flag just typed is boolean (no value slot) — offer the next flag, or positionals if none
		cands = remainingFlags(path, rest)
		if len(cands) == 0 {
			if pos := positionalsFor(path); pos != nil {
				cands = pos()
			}
		}
	case strings.HasPrefix(cur, "-"):
		cands = remainingFlags(path, rest)
	case len(path) == 0 && len(rest) == 0:
		cands = topLevelCandidates(cur)
	case len(rest) == 0:
		cands = subWordsFor(path)
		if pos := positionalsFor(path); pos != nil {
			cands = append(cands, pos()...) // a verb with both (theme: apply + palette names) offers both
		}
	}
	var out []string
	for _, c := range cands {
		if strings.HasPrefix(c, cur) {
			out = append(out, c)
		}
	}
	return out
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
