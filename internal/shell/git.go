package shell

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
func gitValueOpts() map[string]bool { walkerOnce.Do(loadWalker); return walker.gitValueOpts }

// GitValueOpt reports whether a git global option takes the next token as its value (`git -C /repo status`).
func GitValueOpt(name string) bool { return gitValueOpts()[name] }

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
			recordAlias(&aliases, val)
		case "--config-env": // an alias read from an env var is unknowable here: treat it as a `!` alias
			if n, _, found := strings.Cut(val, "="); found {
				recordAlias(&aliases, n+"=!")
			}
		}
	}
	if i < len(toks) {
		verb, args = toks[i], toks[i+1:]
	}
	return gitTarget(cwd, gitDir, workTree, cPaths), verb, args, aliases
}

// recordAlias stores `alias.NAME=VALUE` (the value of a `-c`) into aliases.
func recordAlias(aliases *map[string]string, kv string) {
	if n, v, found := strings.Cut(kv, "="); found && strings.HasPrefix(strings.ToLower(n), "alias.") {
		if *aliases == nil {
			*aliases = map[string]string{}
		}
		(*aliases)[strings.ToLower(n)[len("alias."):]] = v
	}
}

// gitBuiltins are verbs git never lets an alias shadow; the rest of an alias's first word is its real verb.
func gitBuiltins() map[string]bool { walkerOnce.Do(loadWalker); return walker.gitBuiltins }

// expandVerb resolves `-c alias.*`; a `!` alias or a verb the shell expands (`$v`, `{a,b}`) is Uninspectable.
func expandVerb(verb string, args []string, aliases map[string]string) (string, []string) {
	for range maxNest {
		if strings.ContainsAny(verb, "$`{"+subMask) {
			return Uninspectable, nil
		}
		if len(aliases) == 0 || gitBuiltins()[verb] {
			return verb, args
		}
		val, ok := aliases[strings.ToLower(verb)] // git matches alias names case-insensitively
		if !ok {
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
	if gitValueOpts()[t] {
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

// AttributionTrailer reports a line opening with an attribution footer, the shape safety-scan blocks.
func AttributionTrailer(line string) bool { return footerLine(line) }

// SubjectWarnLen is the commit subject length, in characters, above which a message warns (it is never refused).
const SubjectWarnLen = 150

// SubjectLen is a subject's length in characters, not bytes.
func SubjectLen(subject string) int { return utf8.RuneCountInString(subject) }

var conventionalSubject = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(` + strings.Join(GuardWords("conventional_commit_types"), "|") + `)(\(.+\))?!?: \S`)
})

// ConventionalSubject reports a `<type>[(scope)][!]: <description>` subject.
func ConventionalSubject(subject string) bool { return conventionalSubject().MatchString(subject) }

func hasFooter(text string) bool {
	return slices.ContainsFunc(footerPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(text) })
}

// footerLinePrefix opens a line (or follows a `\n` escape): no letter or digit but an attached -m/--trailer= option.
const footerLinePrefix = `(?:^|\\n)[^\p{L}\p{N}\n]*(?:(?:-[a-z]*m|--message=|--trailer=)[^\p{L}\p{N}\n]*)?`

// footerLines are footerPatterns anchored to the start of a line of one argument.
var footerLines = sync.OnceValue(func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, re := range footerPatterns() {
		body := strings.TrimPrefix(re.String(), "(?i)")
		out = append(out, regexp.MustCompile(`(?im)`+footerLinePrefix+`(?:`+body+`)`))
	}
	return out
})

func footerLine(arg string) bool {
	return slices.ContainsFunc(footerLines(), func(re *regexp.Regexp) bool { return re.MatchString(arg) })
}

// maxMessageFile caps how much of an existing -F/-t file is read.
const maxMessageFile = 64 << 10

// messageFileFooter reports a footer line in an existing file named by -F/--file/-t/--template.
func messageFileFooter(target string, args []string) bool {
	for _, path := range messageFiles(target, args) {
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxMessageFile {
			continue
		}
		if b, err := os.ReadFile(path); err == nil && footerLine(string(b)) {
			return true
		}
	}
	return false
}

// messageFiles are the absolute paths of a commit's -F/--file/-t/--template files; stdin (-) is not a file.
func messageFiles(target string, args []string) []string {
	var out []string
	for i, a := range args {
		path := ""
		switch {
		case (a == "-F" || a == "--file" || a == "-t" || a == "--template") && i+1 < len(args):
			path = args[i+1]
		case strings.HasPrefix(a, "--file="), strings.HasPrefix(a, "--template="):
			_, path, _ = strings.Cut(a, "=")
		case len(a) > 2 && (strings.HasPrefix(a, "-F") || strings.HasPrefix(a, "-t")):
			path = a[2:]
		}
		if path != "" && path != "-" {
			out = append(out, resolveIn(target, path))
		}
	}
	return out
}

func isFlag(w string) bool { return strings.HasPrefix(w, "-") }

func resolveIn(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// FooterFileWritten reports a commit message file that the same command writes a footer line into first.
func FooterFileWritten(cmd, cwd string) bool {
	if !containsAny(cmd, "commit", "merge", "tag") || !containsAny(cmd, ">", "tee", "python", "node", "perl", "ruby", "php") ||
		!containsAny(cmd, "-F", "--file", "-t") {
		return false
	}
	files := map[string]bool{}
	EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		for _, p := range messageFiles(target, args) {
			files[p] = files[p] || messageVerb(verb)
		}
		return false
	})
	segs, seps := SegmentsSep(cmd)
	hit := false
	eachSegment(segs, seps, cwd, func(i int, _ []string, dir string, _ bool) {
		hit = hit || slices.ContainsFunc(writtenText(segs, seps, i, dir, files), footerLine)
	})
	return hit
}

// writtenText is what segment i writes into a commit message file: its words, a piped-in source's, its heredoc.
func writtenText(segs, seps []string, i int, dir string, files map[string]bool) []string {
	words, targets := Redirects(segs[i])
	tee := len(words) > 0 && filepath.Base(words[0]) == "tee"
	if tee {
		targets = append(targets, slices.DeleteFunc(slices.Clone(words[1:]), isFlag)...)
	}
	script := interpreterNamesFile(words, files)
	if len(words) == 0 || !script && !slices.ContainsFunc(targets, func(t string) bool { return files[resolveIn(dir, t)] }) {
		return nil
	}
	var text []string
	if !tee {
		text = words[1:]
	}
	text = append(text, stringLiterals(script, words[1:])...)
	text = append(text, pipedAndHeredoc(segs, seps, i)...)
	return text
}

// interpreterNamesFile reports an interpreter command whose code names a message file, so it may write a footer there.
func interpreterNamesFile(words []string, files map[string]bool) bool {
	rest, _ := Unwrap(words, "")
	if len(rest) < 2 || !slices.ContainsFunc([]string{"python", "node", "perl", "ruby", "php"},
		func(n string) bool { return strings.HasPrefix(filepath.Base(rest[0]), n) }) {
		return false
	}
	return slices.ContainsFunc(rest[1:], func(w string) bool {
		for f, ok := range files {
			if ok && strings.Contains(w, f) && writeCall.MatchString(w) {
				return true
			}
		}
		return false
	})
}

// writeCall is a file-write call shape in interpreter code; naming the path alone does not write it.
var writeCall = regexp.MustCompile(`open\([^)]*['"](?:[wax][+bt]*|\+?>>?)['"]|write_text|write_bytes|writeFile|appendFile|createWriteStream|File\.(?:write|open)|file_put_contents|fopen`)

// pipedAndHeredoc is the text segment i is fed: piped-in sources' words and its heredoc body.
func pipedAndHeredoc(segs, seps []string, i int) []string {
	var text []string
	for k := i - 1; k >= 0 && (seps[k] == "|" || seps[k] == "|&"); k-- {
		if w, _ := Redirects(segs[k]); len(w) > 1 {
			text = append(text, w[1:]...)
		}
	}
	if delim, ok := heredocDelim(Tokens(segs[i])); ok {
		k := i + 1
		for k < len(segs) && seps[k-1] != "\n" {
			k++
		}
		for ; k < len(segs) && strings.TrimSpace(segs[k]) != delim; k++ {
			text = append(text, segs[k])
		}
	}
	return text
}

// stringLiterals splits code words at quotes so a footer inside a string literal reads as a line.
func stringLiterals(on bool, words []string) []string {
	var out []string
	if !on {
		return out
	}
	for _, w := range words {
		out = append(out, strings.FieldsFunc(w, func(r rune) bool { return r == '"' || r == '\'' || r == '`' })...)
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}

func messageVerb(verb string) bool {
	return verb == "commit" || verb == "merge" || verb == "tag" || verb == WorktreeCommit
}

// AttributionFooterCommit reports a commit footer at an argument's line start, in raw text past a subject, or built.
func AttributionFooterCommit(cmd, cwd string) bool {
	_, subs := ExtractSubstitutions(cmd)
	dynamic := slices.ContainsFunc(subs, func(s string) bool { return !strings.Contains(s, "<<") })
	var subjects []string
	sawCommit := false
	if EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if messageVerb(verb) || verb == Uninspectable {
			sawCommit = true
		}
		if !messageVerb(verb) {
			return false
		}
		if messageFileFooter(target, args) {
			return true
		}
		for _, a := range args {
			if footerLine(a) {
				return true
			}
			if hasFooter(a) {
				subjects = append(subjects, a)
			}
		}
		return dynamic && slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, subMask) }) &&
			footerMention().MatchString(cmd)
	}) {
		return true
	}
	if !sawCommit {
		return false
	}
	raw := cmd
	for _, s := range subs {
		if strings.Contains(s, "<<") && !strings.ContainsAny(s, "$`") { // a literal heredoc: only a line-start footer counts
			if footerLine(s) {
				return true
			}
			raw = strings.ReplaceAll(raw, s, "")
		}
	}
	for _, s := range subjects {
		raw = strings.ReplaceAll(raw, s, "")
	}
	return hasFooter(raw)
}

// readOnlyGit are the git subcommands a read-only run may shell out to.
var readOnlyGit = sync.OnceValue(func() map[string]bool { return GuardSet("git_sdk_read_only_verbs") })

// gitBannedFlags also match any abbreviation git would accept (--out=x is --output=x).
var gitBannedFlags = sync.OnceValue(func() []string { return GuardWords("git_sdk_banned_flags") })

// gitLeadingFlags must open the verb's args: later, a value option or -- swallows them (git diff -- --no-textconv).
var gitLeadingFlags = sync.OnceValue(func() map[string][]string { return GuardTools("git_sdk_leading_flags") })

// gitVerbFlags are those verbs' only dash args: branch must carry --list or -l, and `git status -v` runs textconv.
var gitVerbFlags = sync.OnceValue(func() map[string]map[string]bool { return GuardToolSets("git_sdk_verb_flags") })

// ReadOnlyGit reports whether cmd is a single allowlisted read-only git invocation (fail closed).
func ReadOnlyGit(cmd string) bool {
	if strings.ContainsFunc(cmd, unsafeGitRune) {
		return false
	}
	f := strings.Fields(cmd)
	if len(f) > 2 && f[0] == "git" && f[1] == "--no-pager" { // exactly one, which copilot adds itself
		f = append([]string{"git"}, f[2:]...)
	}
	if len(f) < 2 || f[0] != "git" || !readOnlyGit()[f[1]] {
		return false
	}
	args, lead := f[2:], gitLeadingFlags()[f[1]]
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
	allowed, list := gitVerbFlags()[verb], false
	for _, a := range args {
		if badGitArg(a, allowed) || escapesArg(a) { // outside paths make diff an implicit --no-index
			return false
		}
		list = list || a == "--list" || a == "-l"
	}
	return (verb != "branch" || list) && (verb != "grep" || grepArgsSafe(args))
}

// escapesArg is a path arg that leaves the checkout, or a short option with one attached (`-O/etc/x`).
func escapesArg(a string) bool {
	if strings.HasPrefix(a, "--") {
		return false
	}
	if strings.HasPrefix(a, "-") {
		return len(a) > 2 && escapesPath(a[2:])
	}
	return escapesPath(a)
}

// gitPathFlags take a file whose content git reads; abbreviations count, like gitBannedFlags.
var gitPathFlags = sync.OnceValue(func() []string { return GuardWords("git_path_flags") })

// pathFlagEscapes is a `--contents=/x`-style arg whose file value leaves the checkout.
func pathFlagEscapes(arg string) bool {
	name, val, found := strings.Cut(arg, "=")
	return found && len(name) > 3 && escapesPath(val) &&
		slices.ContainsFunc(gitPathFlags(), func(f string) bool { return strings.HasPrefix(f, name) })
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
	return slices.ContainsFunc(gitBannedFlags(), func(b string) bool { return strings.HasPrefix(b, name) })
}
