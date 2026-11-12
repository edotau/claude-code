package shell

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

// GitInvocation parses git's global options up to the verb: the target tree, the verb, the verb's words.
func GitInvocation(toks []string, cwd string) (target, verb string, args []string) {
	target, verb, args, _ = parseGit(toks, cwd)
	return target, verb, args
}

// resolvedGit is GitInvocation with the verb run through `-c alias.*` and checked for shell expansion (EachGit's view).
func resolvedGit(toks []string, cwd string) (target, verb string, args []string) {
	target, verb, args, aliases := parseGit(toks, cwd)
	if verb != "" {
		verb, args = expandVerb(verb, args, aliases)
	}
	return target, verb, args
}

func parseGit(toks []string, cwd string) (target, verb string, args []string, aliases map[string]string) {
	var cPaths []string
	var gitDir, workTree string
	aliases = map[string]string{}
	i := 0
	for i < len(toks) && strings.HasPrefix(toks[i], "-") {
		var name, val string
		var ok bool
		if name, val, i, ok = gitGlobalOpt(toks, i); !ok {
			continue
		}
		switch name {
		case "-C":
			cPaths = append(cPaths, val)
		case "--git-dir":
			gitDir = val
		case "--work-tree":
			workTree = val
		case "-c":
			recordAlias(aliases, val)
		}
	}
	if i < len(toks) {
		verb, args = toks[i], toks[i+1:]
	}
	return gitTarget(cwd, gitDir, workTree, cPaths), verb, args, aliases
}

// recordAlias stores `alias.NAME=VALUE` (the value of a `-c`) into aliases.
func recordAlias(aliases map[string]string, kv string) {
	if n, v, found := strings.Cut(kv, "="); found && strings.HasPrefix(strings.ToLower(n), "alias.") {
		aliases[strings.ToLower(n)[len("alias."):]] = v
	}
}

// gitBuiltins are verbs git never lets an alias shadow; the rest of an alias's first word is its real verb.
var gitBuiltins = map[string]bool{"checkout": true, "stash": true, "reset": true, "restore": true, "clean": true,
	"switch": true, "commit": true, "rebase": true, "status": true, "log": true, "add": true, "diff": true}

// expandVerb resolves `-c alias.*`; a `!` alias or a verb the shell expands (`$v`, `{a,b}`) is Uninspectable.
func expandVerb(verb string, args []string, aliases map[string]string) (string, []string) {
	for range maxNest {
		if strings.ContainsAny(verb, "$`{"+subMask) {
			return Uninspectable, nil
		}
		val, ok := aliases[strings.ToLower(verb)] // git matches alias names case-insensitively
		if !ok || gitBuiltins[verb] {
			return verb, args
		}
		words := strings.Fields(val)
		if len(words) == 0 || strings.HasPrefix(val, "!") {
			return Uninspectable, nil
		}
		verb, args = words[0], append(words[1:len(words):len(words)], args...)
	}
	return Uninspectable, nil
}

// gitGlobalOpt splits the global option at toks[i] into name and value, returning the next index; false if valueless.
func gitGlobalOpt(toks []string, i int) (name, val string, next int, ok bool) {
	t := toks[i]
	i++
	if n, v, found := strings.Cut(t, "="); found {
		return n, v, i, true
	}
	if gitValueOpts[t] {
		if i < len(toks) {
			return t, toks[i], i + 1, true
		}
		return t, "", i, true
	}
	if after, found := strings.CutPrefix(t, "-C"); found {
		return "-C", after, i, true
	}
	return "", "", i, false // valueless global option: --no-pager, --bare…
}

// gitTarget is the tree git acts on: --work-tree, else a .git --git-dir's parent, else the -C chain from cwd.
func gitTarget(cwd, gitDir, workTree string, cPaths []string) string {
	base := cwd // successive -C paths apply relative to each other; relative --work-tree/--git-dir follow them
	for _, c := range cPaths {
		if p, ok := resolvePath(c, base); ok {
			base = p
		}
	}
	if p, ok := resolvePath(workTree, base); workTree != "" && ok {
		return p
	}
	if p, ok := resolvePath(gitDir, base); gitDir != "" && ok && filepath.Base(p) == ".git" {
		return filepath.Dir(p)
	}
	return base
}

// gitCommitInvocation reports whether cmd, or a nested/substituted command within it, invokes `git commit`;
// nesting too deep to inspect counts as a possible commit.
func gitCommitInvocation(cmd string) bool {
	return EachGit(cmd, "", func(_, verb string, _ []string) bool { return verb == "commit" || verb == Uninspectable })
}

// footerPatterns match an agent-attribution footer in command text (CLAUDE.md hard rule 3).
var footerPatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)co-authored-by:\s*(claude|anthropic|codex|copilot|gemini|opencode)\b`),
		regexp.MustCompile(`(?i)generated (with|by)\s*\[?(claude( code)?|codex|github copilot|copilot|gemini|opencode)\b`),
	}
})

// footerMention is a looser test used when a message is built by a substitution whose output cannot be seen.
var footerMention = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)co-authored-by|generated (with|by)`)
})

func hasFooter(text string) bool {
	return slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(text) })
}

// AttributionFooterCommit reports a `git commit` carrying a footer in its raw text (heredoc bodies too), in its
// unquoted arguments (`"Cl""aude"`, `$'\x20'`), or in text a substitution builds that mentions one.
func AttributionFooterCommit(cmd string) bool {
	if hasFooter(cmd) && gitCommitInvocation(cmd) {
		return true
	}
	_, subs := ExtractSubstitutions(cmd)
	dynamic := slices.ContainsFunc(subs, func(s string) bool { return !strings.Contains(s, "<<") })
	return EachGit(cmd, "", func(_, verb string, args []string) bool {
		if verb != "commit" {
			return false
		}
		joined := strings.Join(args, " ")
		return hasFooter(joined) || dynamic && strings.Contains(joined, subMask) && footerMention().MatchString(cmd)
	})
}

// readOnlyGit are the git subcommands a read-only run may shell out to.
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "branch": true, "grep": true}

// gitBannedFlags also match any abbreviation git would accept (--out=x is --output=x).
var gitBannedFlags = []string{
	"--output", "--exec", "--ext-diff", "--config", "--open-files-in-pager",
	"--textconv", "--remerge-diff", "--diff-merges", "--submodule", // each still reaches a textconv or merge driver
	"--no-index", // diffs and greps paths outside the repository
}

// gitLeadingFlags must open the verb's args: later, a value option or -- swallows them (git diff -- --no-textconv).
var gitLeadingFlags = map[string][]string{
	"diff": {"--no-ext-diff", "--no-textconv"}, "log": {"--no-ext-diff", "--no-textconv"},
	"show": {"--no-ext-diff", "--no-textconv"}, "blame": {"--no-textconv"},
}

// gitVerbFlags are those verbs' only dash args: branch must carry --list or -l, and `git status -v` runs textconv.
var gitVerbFlags = map[string]map[string]bool{
	"branch": {"--list": true, "-l": true, "-a": true, "--all": true, "-r": true, "--remotes": true, "-v": true, "-vv": true},
	"status": {"--short": true, "-s": true, "--branch": true, "-b": true, "--long": true, "--porcelain": true, "--porcelain=v2": true,
		"-z": true, "--ignored": true, "-uno": true, "-uall": true, "--": true},
}

// ReadOnlyGit reports whether cmd is a single allowlisted read-only git invocation (fail closed).
func ReadOnlyGit(cmd string) bool {
	if strings.ContainsFunc(cmd, unsafeGitRune) {
		return false
	}
	f := strings.Fields(cmd)
	if len(f) > 2 && f[0] == "git" && f[1] == "--no-pager" { // exactly one, which copilot adds itself
		f = append([]string{"git"}, f[2:]...)
	}
	if len(f) < 2 || f[0] != "git" || !readOnlyGit[f[1]] {
		return false
	}
	args, lead := f[2:], gitLeadingFlags[f[1]]
	if len(args) < len(lead) || !slices.Equal(args[:len(lead)], lead) {
		return false
	}
	return readOnlyGitArgs(f[1], args)
}

// unsafeGitRune is any rune outside letters, digits and the plain punctuation a read-only git command needs.
func unsafeGitRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,^-~", r)
}

// readOnlyGitArgs checks verb's args against the banned and per-verb allowed flags; branch must list.
func readOnlyGitArgs(verb string, args []string) bool {
	allowed, list := gitVerbFlags[verb], false
	for _, a := range args {
		if badGitArg(a, allowed) || !strings.HasPrefix(a, "-") && escapesPath(a) { // outside paths make diff an implicit --no-index
			return false
		}
		list = list || a == "--list" || a == "-l"
	}
	return (verb != "branch" || list) && (verb != "grep" || grepArgsSafe(args))
}

// gitPathFlags take a file whose content git reads; abbreviations count, like gitBannedFlags.
var gitPathFlags = []string{"--contents", "--ignore-revs-file"}

// pathFlagEscapes is a `--contents=/x`-style arg whose file value leaves the checkout.
func pathFlagEscapes(arg string) bool {
	name, val, found := strings.Cut(arg, "=")
	return found && len(name) > 3 && escapesPath(val) &&
		slices.ContainsFunc(gitPathFlags, func(f string) bool { return strings.HasPrefix(f, name) })
}

// badGitArg is -c, a ~ path, a banned or outside-file flag, or a dash arg outside the verb's allowed set.
func badGitArg(a string, allowed map[string]bool) bool {
	return a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) || pathFlagEscapes(a) || allowed != nil && strings.HasPrefix(a, "-") && !allowed[a]
}

// grepArgsSafe refuses git grep's pager flag (-O, --open-files-in-pager) and a pattern file (-f) outside the checkout.
func grepArgsSafe(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return true
		case strings.HasPrefix(a, "--"):
			var ok bool
			if i, ok = grepLongFlag(args, i); !ok {
				return false
			}
		case strings.HasPrefix(a, "-"):
			var ok bool
			if i, ok = grepShortCluster(args, i); !ok {
				return false
			}
		}
	}
	return true
}

// grepLongFlag refuses a --file (or --fi.. abbreviation) value outside the checkout; it may consume the next arg.
func grepLongFlag(args []string, i int) (int, bool) {
	name, val, hasEq := strings.Cut(args[i], "=")
	if len(name) < 4 || !strings.HasPrefix("--file", name) { // --fi.. may abbreviate --file
		return i, true
	}
	if !hasEq && i+1 < len(args) {
		i, val = i+1, args[i+1]
	}
	return i, !escapesPath(val)
}

// grepShortCluster walks the short flags of args[i]; a value letter ends the cluster (its value may be the next arg).
func grepShortCluster(args []string, i int) (int, bool) {
	a := args[i]
	for j := 1; j < len(a); j++ {
		switch a[j] {
		case 'O':
			return i, false
		case 'f', 'e', 'A', 'B', 'C', 'm':
			val := a[j+1:]
			if val == "" && i+1 < len(args) {
				i, val = i+1, args[i+1]
			}
			return i, a[j] != 'f' || !escapesPath(val)
		}
	}
	return i, true
}

// escapesPath is a value that is absolute, home-relative or climbs with .. (it may leave every root).
func escapesPath(v string) bool {
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") || slices.Contains(strings.Split(v, "/"), "..")
}

func bannedGitFlag(arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	if !strings.HasPrefix(name, "--") || len(name) < 3 {
		return false
	}
	for _, b := range gitBannedFlags {
		if strings.HasPrefix(b, name) {
			return true
		}
	}
	return false
}
