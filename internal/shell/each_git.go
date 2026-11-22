package shell

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Uninspectable is EachGit's verb (cwd as target) for nesting past maxNest, unseen code a guard must fail closed on.
const Uninspectable = "<uninspectable>"

// WorktreeCommit is EachGit's verb for a `claude-code worktree commit` call; its args are the verb's own words.
const WorktreeCommit = "worktree commit"

const tooDeepArg = "<too-deep>"

// TooDeep reports an Uninspectable call's args as nesting past maxNest, not a verb the shell expands.
func TooDeep(args []string) bool { return len(args) == 1 && args[0] == tooDeepArg }

// gitFn receives one git invocation: its target tree, verb and the verb's words; true stops the walk.
type gitFn func(target, verb string, args []string) bool

// commandVerb is eachGit's internal verb for a command start; its args are the command's words.
const commandVerb = "\x00command"

// EachGit calls fn per git invocation in cmd and its target tree, through nesting, wrappers and cd, until fn is true.
func EachGit(cmd, cwd string, fn gitFn) bool {
	return eachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if verb != commandVerb {
			return fn(target, verb, args)
		}
		if h := HarnessArgs(args); len(h) > 1 && h[0] == "worktree" && h[1] == "commit" {
			return fn(target, WorktreeCommit, h[2:])
		}
		return false
	}, 0, false)
}

// harnessGlobalFlags take a host and precede the verb: remote.Flag (network/remote/ssh.go), egressFlag (cli/table.go).
var harnessGlobalFlags = []string{"--host", "--remote"}

// IsHarnessName reports a command basename that runs claude-code: itself or the .hooks binary that hands over to it.
func IsHarnessName(name string) bool { return name == "claude-code" || name == paths.HookBinaryName }

// HarnessArgs is a claude-code command's verb and its words, past the global host flags; nil for another command.
func HarnessArgs(words []string) []string {
	if len(words) == 0 || !IsHarnessName(filepath.Base(words[0])) {
		return nil
	}
	rest := words[1:]
	for len(rest) > 0 {
		flag, _, attached := strings.Cut(rest[0], "=")
		if !slices.Contains(harnessGlobalFlags, flag) {
			break
		}
		if rest = rest[1:]; !attached && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	return rest
}

// SecretFlag reports --secret on HarnessArgs h; an agents brief is data, so that group never counts.
func SecretFlag(h []string) bool {
	return len(h) > 0 && h[0] != "agents" && BoolFlagOn(h[1:], "secret")
}

// BoolFlagOn reports -name, --name or --name=<not false|0> before any `--`, as Go's flag package reads them.
func BoolFlagOn(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		k, v, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && k == name && (!has || v != "false" && v != "0") {
			return true
		}
	}
	return false
}

// EachCommand calls fn with each command's words and dir, found as EachGit finds git, until fn is true.
func EachCommand(cmd, cwd string, fn func(dir string, words []string) bool) bool {
	return eachGit(cmd, cwd, func(target, verb string, args []string) bool { return verb == commandVerb && fn(target, args) }, 0, false)
}

// eachGit is EachGit at a nesting depth; runs marks text whose output is itself run, so no command in it is inert.
func eachGit(cmd, cwd string, fn gitFn, depth int, runs bool) bool {
	if depth > maxNest {
		return fn(cwd, Uninspectable, []string{tooDeepArg})
	}
	masked, subs := extractSubstitutions(cmd, subMask)
	if len(subs) > 0 {
		executed := executedSubs(masked, len(subs))
		for k, sub := range subs { // a substitution runs nested: its cd stays inside it
			if eachGit(sub, cwd, fn, depth+1, runs || executed[k]) {
				return true
			}
		}
	}
	segs, seps := SegmentsSep(masked)
	var scripts []Script
	if strings.Contains(masked, ">") || strings.Contains(masked, "tee") {
		scripts = runScripts(segs, seps, cwd)
	}
	walk, writing := fn, false
	var seen map[[2]string]bool
	if len(scripts) > 0 {
		seen = map[[2]string]bool{}
		walk = func(target, verb string, args []string) bool {
			if verb != commandVerb {
				seen[[2]string{target, verb}] = seen[[2]string{target, verb}] || writing
			}
			return fn(target, verb, args)
		}
	}
	w := &segWalk{cwd: cwd, segs: segs, seps: seps, words: make([][]string, len(segs)),
		wordsSet: make([]bool, len(segs)), fn: walk, depth: depth, runs: runs}
	for i, seg := range segs {
		w.enter(seg)
		writing = len(scripts) > 0 && w.writesThenRuns(i) // only the writing segment's text repeats in its script
		if w.run(i) {
			return true
		}
		w.leave(seg)
	}
	return nestScripts(scripts, seen, fn, depth, runs)
}

// nestScripts walks each written-then-run script's text, `printf 'git x\n' > f && sh f`, skipping calls already seen.
func nestScripts(scripts []Script, seen map[[2]string]bool, fn gitFn, depth int, runs bool) bool {
	fresh := func(target, verb string, args []string) bool {
		return (verb == commandVerb || !seen[[2]string{target, verb}]) && fn(target, verb, args)
	}
	for _, s := range scripts {
		if eachGit(s.Text, s.Dir, fresh, depth+1, runs) {
			return true
		}
	}
	return false
}

// inertCommands print or search their arguments: a git-looking string there is text, not a command.
func inertCommands() map[string]bool { walkerOnce.Do(loadWalker); return walker.inert }

// inertHarnessVerbs are the claude-code verbs whose arguments are search terms, facts or PR text, never run.
func inertHarnessVerbs() map[string]bool { walkerOnce.Do(loadWalker); return walker.inertHarness }

// inertCommand reports a command whose arguments are text: an inertCommands name or an inertHarnessVerbs verb.
func inertCommand(toks []string) bool {
	if args := HarnessArgs(toks); args != nil {
		return len(args) > 1 && inertHarnessVerbs()[args[0]+" "+args[1]]
	}
	return inertCommands()[filepath.Base(toks[0])]
}

// segmentGit visits the nested command and each git word of one segment whose wrappers are already dropped.
func segmentGit(toks []string, dir string, fn gitFn, depth int, runs bool) bool {
	if toks = DropRtk(toks); len(toks) == 0 {
		return false
	}
	if nested, ok := nestedOf(toks); ok { // its words are the nested command's, visited once there
		return eachGit(nested, dir, fn, depth+1, runs)
	}
	if text, ok := hereString(toks); ok {
		return eachGit(text, dir, fn, depth+1, runs)
	}
	head := toks
	if name := filepath.Base(toks[0]); name == "xargs" || name == "parallel" { // the command they run decides
		head = DropRtk(afterPrefix(toks))
	}
	s := &segScan{dir: dir, fn: fn, depth: depth, runs: runs, inert: !runs && len(head) > 0 && inertCommand(head)}
	for _, st := range CommandStarts(toks) { // each command word, through xargs, parallel, watch, find -exec, go run
		if st = DropRtk(skipEnvAssignments(st)); !s.inert && len(st) > 0 && fn(dir, commandVerb, st) {
			return true
		}
	}
	for i := range toks { // any word: xargs/watch/find -exec need no modelling
		if s.word(toks, i) {
			return true
		}
	}
	return false
}

// segScan is the state of one segmentGit word scan.
type segScan struct {
	dir                   string
	fn                    gitFn
	depth                 int
	runs, inert, afterGit bool
	done                  string // a -c string already visited
}

func (s *segScan) nest(text string) bool { return eachGit(text, s.dir, s.fn, s.depth+1, s.runs) }

// word visits toks[i]: a git word, a shell wrapper's -c string, or a multi-word string that reads as a command line.
func (s *segScan) word(toks []string, i int) bool {
	t := toks[i]
	if !s.inert && (t == "git" || strings.HasSuffix(t, "/git")) && filepath.Base(t) == "git" {
		s.afterGit = true
		return s.fn(resolvedGit(toks[i+1:], s.dir))
	}
	switch {
	case i == 0:
	case shellWrapperNames()[filepath.Base(t)]: // `find -exec sh -c 'STR'`, `setsid sh -c 'STR'`
		if nested, ok := nestedOf(toks[i:]); ok {
			s.done = nested
			return s.nest(nested)
		}
	case cStringCommands()[filepath.Base(toks[0])] && isCFlag(toks[i-1]): // `su -c 'STR'`, `script -qc 'STR'`
		s.done = t
		return s.nest(t)
	case s.textArg(t, toks[0]): // `watch 'git x'`
		return s.nest(t)
	}
	return false
}

// textArg reports a command-line-looking argument that is neither inert, after git's own words, nor already visited.
func (s *segScan) textArg(t, head string) bool {
	if s.afterGit || s.inert || t == s.done || !commandText(t) {
		return false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(t), " ")
	return !IsHarnessName(filepath.Base(first)) || runner(head) // claude-code text runs only under a runner
}

// commandText reports a multi-word token that reads as a command line: its first word is git or a shell wrapper.
func commandText(t string) bool {
	var f [2]string
	n := 0
	for word := range strings.FieldsSeq(t) {
		f[n] = word
		n++
		if n == len(f) {
			break
		}
	}
	if n < 2 {
		return false
	}
	name := filepath.Base(f[0])
	return name == "git" || shellWrapperNames()[name] || IsHarnessName(name) && f[1] == "worktree"
}

// hereString returns the string a bare shell reads from `<<< STR` (also attached `<<<STR`).
func hereString(toks []string) (string, bool) {
	if !bareShell(toks) {
		return "", false
	}
	for i, t := range toks {
		switch {
		case t == "<<<" && i+1 < len(toks):
			return toks[i+1], true
		case strings.HasPrefix(t, "<<<") && len(t) > 3:
			return t[3:], true
		}
	}
	return "", false
}

// pipedIntoShell nests an echo/printf literal piped to a bare shell; any other source is Uninspectable.
func pipedIntoShell(toks []string, dir string, fn gitFn, depth int, runs bool) bool {
	if len(toks) > 0 && (toks[0] == "echo" || toks[0] == "printf") {
		var words []string
		for _, t := range toks[1:] {
			if !strings.HasPrefix(t, "-") {
				words = append(words, t)
			}
		}
		if formatUnapplied(toks[0], words) {
			return fn(dir, Uninspectable, nil)
		}
		return eachGit(strings.Join(words, " "), dir, fn, depth+1, runs)
	}
	return fn(dir, Uninspectable, nil)
}

// formatUnapplied reports echo/printf output the joined text would misread: a backslash escape, or a printf % verb.
func formatUnapplied(verb string, words []string) bool {
	if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, `\`) }) {
		return true
	}
	return verb == "printf" && len(words) > 0 && strings.Contains(words[0], "%")
}
