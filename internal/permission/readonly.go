package permission

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// ReadOnlyShellArg allows, and NotReadOnlyShellArg denies, one Bash segment by the shared read-only shell list.
const (
	ReadOnlyShellArg    = "read-only shell command"
	NotReadOnlyShellArg = "not a read-only shell command"
)

// ReadOnlyAllow is the Bash allow a read-only run adds beside ReadOnlyDeny.
var ReadOnlyAllow = Rule{Tool: "Bash", Arg: ReadOnlyShellArg}

// ReadOnlyShell reports whether every segment of line is on the read-only list, with no file redirect or substitution.
func ReadOnlyShell(line string) bool {
	p := Permissions{Allow: []Rule{ReadOnlyAllow}, Deny: ReadOnlyDeny}
	return p.Decide("Bash", line) == Allow
}

// VerifyShellArg allows, and NotVerifyShellArg denies, one Bash segment by the verify shell: reads, builds, tests.
const (
	VerifyShellArg    = "verify shell command"
	NotVerifyShellArg = "not a verify shell command"
)

// VerifyShell reports whether every segment of line is a read-only or verify command, with no write or substitution.
func VerifyShell(line string) bool {
	allow, deny := Rule{Tool: "Bash", Arg: VerifyShellArg}, Rule{Tool: "Bash", Arg: NotVerifyShellArg}
	return Permissions{Allow: []Rule{allow}, Deny: []Rule{deny}}.Decide("Bash", line) == Allow
}

// verifySegment is a read-only segment or `[GOOS=…] [time] [rtk]` go build|vet|test, gofmt, cd or agents diff.
func verifySegment(seg string, raw bool) bool {
	if !raw {
		return true // the raw spelling, quotes intact, binds: a quote-less re-join loses operand boundaries
	}
	seg, ok := verifySyntax(seg)
	toks := verifyTokens(seg)
	if !ok || len(toks) == 0 || verifyRefused(toks) {
		return false
	}
	if slices.Equal(toks[:min(3, len(toks))], []string{"claude-code", "agents", "diff"}) {
		return verifyCommand(toks) // its own flag list, before the shared readers
	}
	return readOnlySegment(seg, true) || verifyCommand(toks)
}

// verifyRefused are reads the shared list allows that the verify shell does not: a repository the caller picks, a
// flag on an unlisted command (`node --version`) and a `go list` module query, which fetches into the module cache.
func verifyRefused(toks []string) bool {
	return toks[0] == "git" && gitRepoChoice(toks[1:]) || goListFetches(toks) ||
		len(toks) == 2 && strings.HasPrefix(toks[1], "-") && !helpCommand(toks[0])
}

// goListFetches is a `go list` with a module@version query, -versions or -retracted.
func goListFetches(toks []string) bool {
	return len(toks) > 1 && toks[0] == "go" && toks[1] == "list" && slices.ContainsFunc(toks[2:], func(a string) bool {
		name, _, _ := strings.Cut(a, "=")
		return strings.Contains(a, "@") || name == "-versions" || name == "-retracted"
	})
}

// verifyTokens are seg's words past the allowed env and time prefix and one rtk.
func verifyTokens(seg string) []string {
	toks, _ := verifyPrefix(shell.Tokens(seg))
	if len(toks) > 1 && toks[0] == "rtk" {
		return toks[1:]
	}
	return toks
}

// verifySyntax drops one trailing unquoted `2>&1`; any other redirect, a glob, an expansion or other env refuses seg.
func verifySyntax(seg string) (string, bool) {
	seg = trimFdDup(seg)
	at := shell.QuoteAt(seg)
	for i := 0; i < len(seg); i++ {
		if at[i] == 0 && strings.IndexByte("<>*?[", seg[i]) >= 0 {
			return "", false
		}
	}
	_, ok := verifyPrefix(shell.Tokens(seg))
	return seg, ok && !shell.Expands(seg) && !braceExpands(seg)
}

// trimFdDup is seg without a trailing unquoted `2>&1` word; one anywhere else stays and is refused as a redirect.
func trimFdDup(seg string) string {
	t := strings.TrimRight(seg, " \t")
	n := len(t) - 4
	if n < 1 || t[n:] != "2>&1" || t[n-1] != ' ' && t[n-1] != '\t' || string(shell.QuoteAt(t)[n:]) != "\x00\x00\x00\x00" {
		return seg
	}
	return t[:n]
}

// gitRepoChoice is a git global option before the verb that picks the repository, its config or git's programs:
// a configured diff or textconv helper of a repository the caller chose would run.
func gitRepoChoice(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch {
		case !strings.HasPrefix(a, "-"):
			return false
		case strings.HasPrefix(a, "-C") || strings.HasPrefix(a, "-c") ||
			slices.Contains([]string{"--git-dir", "--work-tree", "--namespace", "--config-env", "--exec-path"}, name):
			return true
		}
	}
	return false
}

// verifyCommand judges a prefix-free verify command by its name.
func verifyCommand(toks []string) bool {
	switch toks[0] {
	case "go":
		return verifyGo(toks[1:])
	case "gofmt":
		return verifyGofmt(toks[1:])
	case "claude-code":
		return len(toks) > 2 && toks[1] == "agents" && toks[2] == "diff" && detectorArgsOK(toks[3:], []string{"--check"},
			map[string]func(string) bool{"--path": func(v string) bool { return !strings.HasPrefix(v, "-") }})
	case "cd":
		return len(toks) == 2 && !strings.HasPrefix(toks[1], "-")
	}
	return false
}

// verifyGofmt is gofmt with -l or -d (plus -s, -e) before at least one path: gofmt stops reading flags at the first.
func verifyGofmt(args []string) bool {
	lists, i := false, 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-l", "-d":
			lists = true
		case "-s", "-e":
		default:
			return false
		}
	}
	return lists && i < len(args) && !slices.ContainsFunc(args[i:], func(a string) bool { return strings.HasPrefix(a, "-") })
}

var verifyEnv = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(GOOS|GOARCH|CGO_ENABLED)=[A-Za-z0-9_]*$`)
})

// verifyPrefix is toks past GOOS, GOARCH and CGO_ENABLED assignments and a time word; false for any other assignment.
func verifyPrefix(toks []string) ([]string, bool) {
	for len(toks) > 0 && (toks[0] == "time" || envAssign().MatchString(toks[0])) {
		if toks[0] != "time" && !verifyEnv().MatchString(toks[0]) {
			return nil, false
		}
		toks = toks[1:]
	}
	return toks, true
}

// goVerifyFlags are the only flags each verify verb takes; a nil check marks a boolean flag, which takes no value.
var goVerifyFlags = map[string]map[string]func(string) bool{
	"build": {"-o": func(v string) bool { return v == os.DevNull }},
	"vet":   {},
	"test": {"-run": func(v string) bool { return v != "" }, "-count": regexp.MustCompile(`^[0-9]+$`).MatchString,
		"-v": nil, "-race": nil},
}

// verifyGo is go build|vet|test with listed flags and at least one package.
func verifyGo(args []string) bool {
	if len(args) < 2 || goVerifyFlags[args[0]] == nil {
		return false
	}
	flags, pkgs := goVerifyFlags[args[0]], 0
	for rest := args[1:]; len(rest) > 0; {
		n, ok := 1, goPackageArg(args[0], rest[0])
		if strings.HasPrefix(rest[0], "-") {
			n, ok = goFlag(flags, rest)
		} else {
			pkgs++
		}
		if !ok {
			return false
		}
		rest = rest[n:]
	}
	return pkgs > 0
}

// goFlag judges the flag at rest[0]; n is how many words it takes.
func goFlag(flags map[string]func(string) bool, rest []string) (n int, ok bool) {
	name, val, hasVal := strings.Cut(rest[0], "=")
	valid, known := flags[name]
	switch {
	case !known:
		return 0, false
	case valid == nil:
		return 1, !hasVal
	case hasVal:
		return 1, valid(val)
	case len(rest) > 1:
		return 2, valid(rest[1])
	}
	return 0, false
}

var goPackageWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_./-]+$`) })

// goPackageArg is a `./` path with no `..` or empty element; `/...` needs an element before it, but build and vet
// may take `./...` (a test never runs the whole module: R16).
func goPackageArg(verb, a string) bool {
	rest, ok := strings.CutPrefix(a, "./")
	if !ok || !goPackageWord().MatchString(a) {
		return false
	}
	if rest == "..." {
		return verb != "test"
	}
	return !slices.ContainsFunc(strings.Split(strings.TrimSuffix(rest, "/..."), "/"), func(e string) bool {
		return e == "" || e == "." || strings.Contains(e, "..")
	})
}

// ReadOnlyBashHeads are the command prefixes of the list, for an agent whose native matcher takes "<head> *" patterns.
func ReadOnlyBashHeads() []string {
	heads := []string{"rg", "grep", "ls", "wc", "head", "tail", "cat", "sed -n", "go doc", "go list", "go env", "go version",
		"git branch --list"}
	for _, v := range gitPlainVerbs() {
		if !slices.Contains(gitGatedVerbs(), v) {
			heads = append(heads, "git "+v)
		}
	}
	verbs := readOnlyClaudeVerbs()
	for _, group := range slices.Sorted(maps.Keys(verbs)) {
		for _, v := range verbs[group] {
			heads = append(heads, "claude-code "+group+" "+v)
		}
	}
	return heads
}

var helpWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) })

// readOnlySegment judges one segment; raw is the as-typed spelling (quotes intact), else the quote-stripped one.
func readOnlySegment(seg string, raw bool) bool {
	rawToks := shell.Tokens(seg)
	toks, _ := shell.Unwrap(rawToks, "")
	if len(toks) == 0 || raw && !rawSegmentOK(seg, rawToks, toks) || slices.ContainsFunc(rawToks[:len(rawToks)-len(toks)], isEnvWord) {
		return false
	}
	toks = rtkPlain(toks)
	if len(toks) == 2 && helpFlag(toks[0], toks[1]) || claudeHelp(toks) {
		return true
	}
	return readOnlyCommand(toks)
}

// helpFlag is `--help` or `--version`, which print and exit, or `-h` on a listed reader only: `bash -h` is hashing
// and still runs stdin.
func helpFlag(name, flag string) bool {
	switch flag {
	case "--help", "--version":
		return helpWord().MatchString(name)
	case "-h":
		return helpCommand(name)
	}
	return false
}

// helpCommand is a listed reader whose -h prints help.
func helpCommand(name string) bool {
	return plainReadCommands()[name] || readOnlyCLIs()[name] != nil ||
		slices.Contains([]string{"git", "go", "gofmt", "rg", "find", "head", "tail", "sed", "rtk", "claude-code"}, name)
}

// isEnvWord is the `env` wrapper, which prints the environment (credentials) or runs a command: refused in every form.
func isEnvWord(t string) bool { return filepath.Base(t) == "env" }

// rtkPlain is the plain command `rtk <sub> …` stands for; claude-code is no rtk subcommand, so rtk runs it as typed.
func rtkPlain(toks []string) []string {
	if len(toks) < 2 || toks[0] != "rtk" {
		return toks
	}
	if rtkPlainSubcommands()[toks[1]] {
		return toks[1:]
	}
	if toks[1] == "read" {
		return append([]string{"cat"}, toks[2:]...)
	}
	return toks
}

var rtkPlainSubcommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("read_only_rtk_plain_subcommands") })

// claudeHelp is `claude-code <verb> [<subverb>] -h|--help`: help text never writes.
func claudeHelp(toks []string) bool {
	n := len(toks)
	if n < 3 || n > 4 || toks[0] != "claude-code" || toks[n-1] != "-h" && toks[n-1] != "--help" {
		return false
	}
	return !slices.ContainsFunc(toks[1:n-1], func(t string) bool { return !helpWord().MatchString(t) })
}

// rawSegmentOK is the as-typed part of the check: no write, `<` or expansion, and only a `time` or locale prefix.
func rawSegmentOK(seg string, rawToks, toks []string) bool {
	if hasWrite(seg) || shell.Expands(seg) || braceExpands(seg) {
		return false
	}
	if name := rtkPlain(toks)[0]; slices.Contains([]string{"rg", "find", "git", "go", "claude-code"}, name) || readOnlyCLIs()[name] != nil {
		// a glob leading a word, or in a word that starts with -, can expand to a flag (a file named --pre=x)
		at, lead, dash := shell.QuoteAt(seg), true, false
		for i := 0; i < len(seg); i++ {
			switch c := seg[i]; {
			case at[i] == 0 && (c == ' ' || c == '\t' || c == '\n'):
				lead, dash = true, false
			case at[i] == 0 && strings.IndexByte("*?[", c) >= 0 && (lead || dash):
				return false
			case c != '\'' && c != '"' && c != '\\':
				dash, lead = lead && c == '-' || dash, false
			}
		}
	}
	n := len(rawToks) - len(toks)
	return n == 0 || !slices.ContainsFunc(rawToks[:n], func(t string) bool { return t != "time" && !localeAssign().MatchString(t) })
}

// localeAssign is the one prefix assignment a read may carry: a locale, time zone or display setting, no path.
var localeAssign = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(LANG|LC_[A-Z]+|TZ|NO_COLOR|COLUMNS)=[A-Za-z0-9._@-]*$`)
})

// braceExpands is an unquoted `{a,b}` or `{1..3}`, which bash turns into several words the check never sees.
func braceExpands(seg string) bool {
	at, open, list := shell.QuoteAt(seg), false, false
	for i := 0; i < len(seg); i++ {
		if at[i] != 0 {
			continue
		}
		switch c := seg[i]; {
		case c == '{':
			open, list = true, false
		case c == ',' || c == '.' && i+1 < len(seg) && seg[i+1] == '.':
			list = list || open
		case c == '}' && list:
			return true
		case c == '}' || c == ' ' || c == '\t' || c == '\n':
			open = false
		}
	}
	return false
}

// readOnlyCommand judges a wrapper-free command by its name.
func readOnlyCommand(toks []string) bool {
	if plainReadCommands()[toks[0]] {
		return true
	}
	switch toks[0] {
	case "git":
		return readOnlyGit(toks)
	case "claude-code":
		return readOnlyClaudeCode(toks[1:])
	case "go":
		return readOnlyGo(toks[1:])
	case "rg":
		return noFlag(toks[1:], bannedFlags()["rg"]...)
	case "printf": // -v assigns a variable, and bash evaluates an array subscript in its name
		return len(toks) < 2 || !strings.HasPrefix(toks[1], "-v")
	case "find":
		return noFlag(toks[1:], findActions()...)
	case "head", "tail":
		return noFlag(toks[1:], bannedFlags()[toks[0]]...)
	case "sed":
		return sedPrintMatches(strings.Join(toks, " "))
	}
	return readOnlyCLI(toks)
}

var plainReadCommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("read_only_plain_commands") })

// bannedFlags are the flags refused on rg, head, tail and the go and claude-code verbs that are otherwise read-only.
var bannedFlags = sync.OnceValue(func() map[string][]string { return shell.GuardTools("read_only_banned_flags") })

// findActions are the find tokens that write a file or run a command.
var findActions = sync.OnceValue(func() []string {
	return slices.Concat(shell.GuardWords("find_exec_flags"), shell.GuardWords("find_write_flags"))
})

// hasWrite is a raw-segment property: a file redirect, or an unquoted `<` (heredoc, here-string, process substitution).
func hasWrite(seg string) bool {
	if shell.FileRedirect(seg) {
		return true
	}
	at := shell.QuoteAt(seg)
	for i := 0; i < len(seg); i++ {
		if seg[i] == '<' && at[i] == 0 {
			return true
		}
	}
	return false
}

// noFlag is true when no arg is a banned flag, by name (any dashes, =value) or short letter in a cluster.
func noFlag(args []string, banned ...string) bool {
	for _, a := range args {
		for _, b := range banned {
			if flagIs(a, b) {
				return false
			}
		}
	}
	return true
}

func flagIs(a, banned string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	want := strings.TrimLeft(banned, "-")
	if len(want) == 1 {
		return !strings.HasPrefix(a, "--") && strings.Contains(a, want)
	}
	name, _, _ := strings.Cut(a, "=")
	return strings.TrimLeft(name, "-") == want
}

// gitPlainVerbs are the git subcommands a read-only run may shell out to, in their plain spelling.
var gitPlainVerbs = sync.OnceValue(func() []string { return shell.GuardWords("git_read_only_verbs") })

// gitGatedVerbs are the gitPlainVerbs that also write, so only the read forms their arg check names pass.
var gitGatedVerbs = sync.OnceValue(func() []string { return shell.GuardWords("git_read_only_gated_verbs") })

// gitBannedLong lets the caller pick a program, config, repo or output file (abbreviations too); not shell.ReadOnlyGit.
var gitBannedLong = sync.OnceValue(func() []string { return shell.GuardWords("git_read_only_banned_long") })

// readOnlyGit is plain git: configured drivers are the owner's; the caller picks no program, config, repo or output.
func readOnlyGit(toks []string) bool {
	rest, dirs := gitGlobalOpts(toks[1:])
	if len(rest) == 0 || !slices.Contains(gitPlainVerbs(), rest[0]) || !gitArgsOK(rest) || len(dirs) > 0 && noIndexDiff(rest) {
		return false
	}
	switch rest[0] {
	case "ls-files":
		return lsFilesArgs(rest[1:])
	case "branch":
		return branchArgs(rest[1:])
	case "remote":
		return remoteArgs(rest[1:])
	case "worktree":
		return worktreeArgs(rest[1:])
	case "stash":
		return len(rest) > 1 && (rest[1] == "list" || rest[1] == "show")
	case "clean":
		return cleanArgs(rest[1:])
	case "reflog":
		return len(rest) == 1 || rest[1] == "show" || strings.HasPrefix(rest[1], "-")
	}
	return true
}

// gitGlobalOpts strips leading --no-pager, --no-optional-locks and `-C <dir>`, returning the dirs.
func gitGlobalOpts(rest []string) ([]string, []string) {
	var dirs []string
	for len(rest) > 0 {
		switch {
		case rest[0] == "--no-pager" || rest[0] == "--no-optional-locks":
			rest = rest[1:]
		case rest[0] == "-C" && len(rest) > 2 && !strings.HasPrefix(rest[1], "-"):
			dirs, rest = append(dirs, rest[1]), rest[2:]
		default:
			return rest, dirs
		}
	}
	return rest, dirs
}

// noIndexDiff is a -C command that is, or outside a repo becomes, a --no-index read of files in that directory.
func noIndexDiff(rest []string) bool {
	n, ops := 0, false
	for _, a := range rest[1:] {
		name, _, _ := strings.Cut(a, "=")
		switch {
		case !ops && a == "--":
			ops = true
		case !ops && len(name) >= 4 && strings.HasPrefix("--no-index", name):
			return true
		case ops || !strings.HasPrefix(a, "-"):
			n++
		}
	}
	return rest[0] == "diff" && n >= 2
}

// gitArgsOK judges the words after the verb at rest[0].
func gitArgsOK(rest []string) bool {
	for _, a := range rest[1:] {
		name, _, _ := strings.Cut(a, "=")
		letters := ""
		if rest[0] == "grep" && len(a) > 1 && a[0] == '-' && a[1] != '-' {
			letters = a[1:]
			if i := strings.IndexAny(letters, "efABC"); i >= 0 { // these take the rest of the word as their value
				letters = letters[:i]
			}
		}
		switch {
		case strings.Contains(letters, "O"):
			return false // git grep -O runs a pager program
		case strings.HasPrefix(a, "--"):
			if slices.ContainsFunc(gitBannedLong(), func(b string) bool { return len(name) >= 4 && name != "--text" && strings.HasPrefix(b, name) }) {
				return false
			}
		case !strings.HasPrefix(a, "-") && escapesCheckout(a):
			return false
		}
	}
	return true
}

// escapesCheckout is a path argument that is absolute, home-relative or climbs out: an implicit `--no-index` target.
func escapesCheckout(a string) bool {
	return strings.HasPrefix(a, "/") || strings.HasPrefix(a, "~") || slices.Contains(strings.Split(a, "/"), "..")
}

var lsFilesFlags = sync.OnceValue(func() []string { return shell.GuardWords("git_ls_files_flags") })

func lsFilesArgs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !slices.Contains(lsFilesFlags(), a) {
			return false
		}
	}
	return true
}

// branchArgs is `git branch` only as a listing; a name operand is a pattern only beside a list or filter flag.
func branchArgs(args []string) bool {
	list, operand := false, false
	for _, a := range args {
		name, _, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--list" || a == "-l" || slices.Contains([]string{"--contains", "--merged", "--no-merged"}, name):
			list = true
		case slices.Contains([]string{"--format", "--sort"}, name):
			if !hasVal {
				return false
			}
		case slices.Contains([]string{"--show-current", "--all", "--remotes"}, a):
		case len(a) > 1 && a[0] == '-' && strings.Trim(a[1:], "arvl") == "":
			list = list || strings.Contains(a, "l")
		case strings.HasPrefix(a, "-"):
			return false
		default:
			operand = true
		}
	}
	return list || !operand
}

// remoteArgs is bare `git remote`, `-v`, or `show|get-url <name>` with a plain name (a URL would be fetched).
func remoteArgs(args []string) bool {
	switch {
	case len(args) == 0:
		return true
	case len(args) == 1:
		return args[0] == "-v" || args[0] == "--verbose"
	}
	return len(args) == 2 && (args[0] == "show" || args[0] == "get-url") && helpWord().MatchString(args[1])
}

// worktreeArgs is `git worktree list` with its display flags.
func worktreeArgs(args []string) bool {
	return len(args) > 0 && args[0] == "list" && !slices.ContainsFunc(args[1:], func(a string) bool {
		return !slices.Contains(worktreeListFlags(), a)
	})
}

var worktreeListFlags = sync.OnceValue(func() []string { return shell.GuardWords("git_worktree_list_flags") })

// cleanArgs is `git clean` only with a dry-run flag, which git honors over -f; -i and -e are out.
func cleanArgs(args []string) bool {
	dry := false
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dry = true
		case a == "--force" || a == "--quiet":
		case strings.HasPrefix(a, "--"):
			return false
		case strings.HasPrefix(a, "-") && strings.Trim(a[1:], "ndxXqf") == "" && len(a) > 1:
			dry = dry || strings.Contains(a, "n")
		case strings.HasPrefix(a, "-"):
			return false
		}
	}
	return dry
}

// writeFlags are option names that suggest an output file, a repair or a deletion; any one denies a claude-code verb.
var writeFlags = sync.OnceValue(func() []string { return shell.GuardWords("claude_code_write_flags") })

var readOnlyClaudeVerbs = sync.OnceValue(func() map[string][]string { return shell.GuardTools("claude_code_read_only_verbs") })

// readOnlyClaudeCode is a listed `claude-code <group> <verb>` with no write-looking flag or --secret.
func readOnlyClaudeCode(args []string) bool {
	if len(args) >= 2 && args[0] == "agents" && args[1] == "sync" { // only its --check and --dry-run forms write nothing
		return slices.ContainsFunc(args[2:], func(a string) bool { return a == "--check" || a == "--dry-run" }) &&
			detectorArgsOK(args[2:], []string{"--check", "--dry-run"}, map[string]func(string) bool{
				"--agent": helpWord().MatchString, "--format": func(v string) bool { return v == "text" || v == "json" }})
	}
	if len(args) < 2 || !slices.Contains(readOnlyClaudeVerbs()[args[0]], args[1]) || shell.SecretFlag(args) {
		return false
	}
	switch args[0] + " " + args[1] {
	case "review simplify":
		return simplifyArgsOK(args[2:])
	case "review scope":
		return scopeArgsOK(args[2:])
	case "models preflight":
		return noFlag(args[2:], bannedFlags()["models preflight"]...)
	case "agents diff":
		return noFlag(args[2:], append(slices.Clone(writeFlags()), "--check")...)
	}
	return noFlag(args[2:], writeFlags()...)
}

// unsafeSimplifyRune is any rune outside letters, digits and the plain punctuation a path or ref needs.
func unsafeSimplifyRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,-", r)
}

// simplifyArgsOK allows only --json, --fail, --threshold <level> and --base <ref> plus local paths.
func simplifyArgsOK(args []string) bool {
	return detectorArgsOK(args, []string{"--json", "--fail"}, map[string]func(string) bool{
		"--threshold": func(v string) bool { return slices.Contains([]string{"strict", "medium", "relaxed"}, v) },
		"--base":      func(v string) bool { return !strings.HasPrefix(v, "-") },
	})
}

// scopeArgsOK allows only review scope's --caps, --importers, --format, --range and --paths flags (never --repo).
func scopeArgsOK(args []string) bool {
	return detectorArgsOK(args, []string{"--caps", "--importers"}, map[string]func(string) bool{
		"--format": func(v string) bool { return v == "text" || v == "json" },
		"--range":  func(v string) bool { return !strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "~") },
		"--paths": func(v string) bool {
			return !strings.Contains(v, "~") && !slices.ContainsFunc(scopeOperands([]string{"--paths", v}), notLocal)
		},
	}, '~', '^')
}

// detectorArgsOK allows the bool flags, valued flags whose value passes, and local paths; extra runes count as safe.
func detectorArgsOK(args, bools []string, valueOK map[string]func(string) bool, extra ...rune) bool {
	unsafe := func(r rune) bool { return unsafeSimplifyRune(r) && !slices.Contains(extra, r) }
	for ; len(args) > 0; args = args[1:] {
		a := args[0]
		if strings.ContainsFunc(a, unsafe) {
			return false
		}
		if ok, isValued := valueOK[a]; isValued {
			if !valuedArgOK(args, ok, unsafe) {
				return false
			}
			args = args[1:]
		} else if !plainArgOK(a, bools) {
			return false
		}
	}
	return true
}

// valuedArgOK: the flag at args[0] has a value at args[1] that passes ok and holds no unsafe rune.
func valuedArgOK(args []string, ok func(string) bool, unsafe func(rune) bool) bool {
	return len(args) >= 2 && ok(args[1]) && !strings.ContainsFunc(args[1], unsafe)
}

// plainArgOK: a listed bool flag or a local path operand.
func plainArgOK(a string, bools []string) bool {
	return slices.Contains(bools, a) || (!strings.HasPrefix(a, "-") && filepath.IsLocal(a))
}

// readOnlyGo is go doc|list|env|version with no flag that builds, downloads, runs a tool or writes the environment.
func readOnlyGo(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "doc", "version":
		return true
	case "env":
		return noFlag(args[1:], bannedFlags()["go env"]...)
	case "list":
		return noFlag(args[1:], bannedFlags()["go list"]...)
	}
	return false
}

// opDir is the dir a segment's relative operands name; "" is the process cwd unless lost (a cd moved it, unknown).
type opDir struct {
	dir  string
	lost bool
}

// readOnlyReadDenied: a Read deny exists and the segment's operand matches it or its operands cannot be listed.
func readOnlyReadDenied(p Permissions, seg string, at opDir) bool {
	if !slices.Contains(p.Allow, ReadOnlyAllow) {
		return false
	}
	var reads []Rule
	for _, r := range p.Deny {
		if r.Tool == "Read" {
			reads = append(reads, r)
		}
	}
	if len(reads) == 0 {
		return false
	}
	toks, _ := shell.Unwrap(shell.Tokens(seg), "")
	toks = rtkPlain(toks)
	if len(toks) > 0 && slices.Contains(noFileCommands(), toks[0]) {
		return false
	}
	if len(toks) > 0 && toks[0] == "cd" {
		return cdReadDenied(reads, toks)
	}
	ops, ok := readOperands(toks, expansionsQuoted(seg))
	if !ok || slices.ContainsFunc(reads, func(r Rule) bool { return r.Arg == "" }) {
		return true
	}
	deny := Permissions{Deny: reads}
	return slices.ContainsFunc(ops, func(o string) bool {
		// after a cd to an unknown dir a climbing operand cannot be placed
		return at.lost && climbs(o) && !path.IsAbs(filepath.ToSlash(o)) ||
			slices.ContainsFunc(operandForms(o, at.dir), func(f string) bool { return deny.Decide("Read", f) == Deny })
	})
}

func climbs(o string) bool { return slices.Contains(strings.Split(filepath.ToSlash(o), "/"), "..") }

// noFileCommands read no file: builtins whose arguments are printed, never opened (a redirect never reaches here).
var noFileCommands = sync.OnceValue(func() []string { return shell.GuardWords("no_file_commands") })

// cdReadDenied refuses a cd that is not one literal absolute path, or whose target a Read deny covers or sits above.
func cdReadDenied(reads []Rule, toks []string) bool {
	if len(toks) != 2 || !path.IsAbs(filepath.ToSlash(toks[1])) || strings.ContainsAny(toks[1], "$`*?[{~\\") {
		return true
	}
	target := path.Clean(toks[1])
	targets := []string{target, paths.CanonDeep(target)}
	return slices.ContainsFunc(reads, func(r Rule) bool {
		dirs := append(denyParentDirs(r.Arg), denyParentDirs(ExpandHome("Read", r.Arg))...)
		return slices.ContainsFunc(targets, func(t string) bool {
			return r.matches("Read", t) || slices.ContainsFunc(dirs, func(d string) bool { return pathMatches(d, t) })
		})
	})
}

// denyParentDirs are the directory globs a cd may not reach, since a relative operand there would name the denied path.
func denyParentDirs(pattern string) []string {
	p := paths.CleanGlob(pattern)
	if !path.IsAbs(p) && !strings.HasPrefix(p, "**/") {
		p = "**/" + p // a relative rule may match under any cwd
	}
	var dirs []string
	for d := path.Dir(p); d != "**" && d != "."; d = path.Dir(d) {
		dirs = append(dirs, d)
		if d == "/" {
			break
		}
	}
	return dirs
}

// operandForms: o as typed, cleaned, joined to dir (else cwd), symlink-resolved if it climbs, and its sub-paths.
func operandForms(o, dir string) []string {
	c := path.Clean(o)
	forms := []string{o, c}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	abs := o
	if !path.IsAbs(c) && dir != "" {
		abs = filepath.ToSlash(dir) + "/" + o
		forms = append(forms, path.Clean(abs))
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil && climbs(o) {
		forms = append(forms, filepath.ToSlash(real))
	}
	parts := strings.Split(c, "/")
	for i := 1; i < len(parts); i++ {
		forms = append(forms, strings.Join(parts[i:], "/"))
	}
	return forms
}

// patchFlags make git log print file contents.
var patchFlags = sync.OnceValue(func() []string { return shell.GuardWords("git_log_patch_flags") })

const expansionChars = "*?[{$~"

// expansionsQuoted: every expansion char in seg sits in plain '…' or after a backslash, so bash reads it literally.
func expansionsQuoted(seg string) bool {
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case strings.IndexByte(expansionChars, c) >= 0: // "…" still expands $; $'…' may spell any byte
			return false
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		}
	}
	return true
}

// readOperands are the file operands of a read-only command; ok is false when they cannot be listed reliably.
func readOperands(toks []string, literal bool) (ops []string, ok bool) {
	if len(toks) == 0 {
		return nil, false
	}
	args := toks[1:]
	files := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "-") })
	if !literal && slices.ContainsFunc(files, func(f string) bool { return strings.ContainsAny(f, expansionChars) }) {
		return nil, false // a glob or expansion names files this check cannot see
	}
	return commandOperands(toks[0], args, files)
}

// commandOperands maps one command name to its file operands.
func commandOperands(name string, args, files []string) ([]string, bool) {
	switch name {
	case "cat", "head", "tail", "wc", "ls", "sed":
		return files, true
	case "grep":
		return files, !slices.ContainsFunc(args, recursiveGrep)
	case "git":
		return gitReadOperands(args)
	case "claude-code":
		return claudeReadOperands(args, files)
	case "go":
		return nil, true
	}
	_, cli := readOnlyCLIs()[name]
	return nil, cli // rg and find search the tree; a cluster or cloud CLI reads no repo path
}

func recursiveGrep(a string) bool {
	return a == "--recursive" || a == "--dereference-recursive" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
		strings.ContainsAny(a, "rR")
}

// gitReadOperands: status, rev-parse, ls-files, branch and a patch-less log show no contents; others cannot be mapped.
func gitReadOperands(args []string) ([]string, bool) {
	args, dirs := gitGlobalOpts(args)
	for len(args) > 0 && strings.HasPrefix(args[0], "--no-") {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "status", "rev-parse", "ls-files", "branch", "remote", "worktree":
		return dirs, true
	case "log":
		return dirs, !slices.ContainsFunc(args[1:], func(a string) bool {
			return slices.Contains(patchFlags(), a) || strings.HasPrefix(a, "-L") || strings.HasPrefix(a, "--patch")
		})
	}
	return nil, false
}

// claudeReadOperands: agents find|diff read the tree, review simplify|scope their files; the rest read no repo path.
func claudeReadOperands(args, files []string) ([]string, bool) {
	if len(args) < 2 {
		return nil, false
	}
	switch args[0] + " " + args[1] {
	case "agents find", "agents diff":
		return nil, false
	case "review simplify":
		return files[2:], true
	case "review scope":
		return scopeOperands(args[2:]), true
	}
	return nil, true
}

// scopeOperands are the comma-separated paths of review scope's --paths flag.
func scopeOperands(args []string) []string {
	i := slices.Index(args, "--paths")
	if i < 0 || i+1 >= len(args) {
		return nil
	}
	return strings.Split(args[i+1], ",")
}

func notLocal(p string) bool { return !filepath.IsLocal(p) }

// cliForm is a read-only subcommand (one or two words) of a cluster or cloud CLI and the only flags it may carry.
type cliForm struct {
	words string
	flags map[string]string // flag: "" takes no value, "*" any value, "/" an owner/repo, else `|`-separated values
}

// cliFlags parses `-x` (no value), `--name=` (any value) and `--name=a|b` entries into a flag table.
func cliFlags(spec []string) map[string]string {
	flags := map[string]string{}
	for _, f := range spec {
		name, val, hasVal := strings.Cut(f, "=")
		if flags[name] = val; hasVal && val == "" {
			flags[name] = "*"
		}
	}
	return flags
}

// cliTools are the tools a table row keyed by tool names: a kubectl row is every kube CLI's (oc too).
func cliTools(tool string) []string {
	if tool == "kubectl" {
		return shell.GuardWords("kube_clis")
	}
	return []string{tool}
}

// readOnlyCLIs is the (tool, subcommand) table: nothing is allowed by tool name alone; kubectl config view is out.
var readOnlyCLIs = sync.OnceValue(func() map[string][]cliForm {
	out := map[string][]cliForm{}
	add := func(key string, spec []string) {
		tool, words, _ := strings.Cut(key, " ")
		for _, t := range cliTools(tool) {
			out[t] = append(out[t], cliForm{words, cliFlags(spec)})
		}
	}
	for key, spec := range shell.GuardTools("read_only_cli_forms") {
		add(key, spec)
	}
	for _, key := range shell.GuardWords("read_only_cli_bare_forms") {
		add(key, nil)
	}
	return out
})

// cliGlobals are the value flags a tool may carry before its subcommand; any other leading flag refuses the command.
var cliGlobals = sync.OnceValue(func() map[string][]string {
	out := map[string][]string{}
	for tool, flags := range shell.GuardTools("read_only_cli_global_flags") {
		for _, t := range cliTools(tool) {
			out[t] = flags
		}
	}
	return out
})

var kubeCLIs = sync.OnceValue(func() map[string]bool { return shell.GuardSet("kube_clis") })

// readOnlyCLI judges `<tool> [globals] <subcommand> …` against readOnlyCLIs.
func readOnlyCLI(toks []string) bool {
	forms, ok := readOnlyCLIs()[toks[0]]
	if !ok {
		return false
	}
	rest, ok := skipCLIGlobals(toks[0], toks[1:])
	if !ok || slices.ContainsFunc(rest, secretKind) && kubeCLIs()[toks[0]] {
		return false
	}
	return slices.ContainsFunc(forms, func(f cliForm) bool {
		w := strings.Fields(f.words)
		return len(rest) >= len(w) && slices.Equal(rest[:len(w)], w) && cliFlagsOK(rest[len(w):], f.flags)
	})
}

// skipCLIGlobals drops the leading value flags the tool allows (`-n ns`, `--context=x`); a bad value is not ok.
func skipCLIGlobals(tool string, rest []string) ([]string, bool) {
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		name, val, hasVal := strings.Cut(rest[0], "=")
		if !slices.Contains(cliGlobals()[tool], name) {
			return rest, true
		}
		rest = rest[1:]
		if !hasVal {
			if len(rest) == 0 {
				return nil, false
			}
			val, rest = rest[0], rest[1:]
		}
		if !helpWord().MatchString(val) {
			return nil, false
		}
	}
	return rest, true
}

// cliFlagsOK: each flag is listed, a value flag has a passing value, and a short cluster passes shortCluster.
func cliFlagsOK(args []string, flags map[string]string) bool {
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		want, known := flags[name]
		switch {
		case !strings.HasPrefix(name, "-"): // an operand: a name, number or kind
		case known && want == "":
			if hasVal {
				return false
			}
		case known:
			if !hasVal && i+1 < len(args) {
				i++
				val, hasVal = args[i], true
			}
			if !hasVal || !cliValueOK(want, val) {
				return false
			}
		case !shortCluster(args[i], flags):
			return false
		}
	}
	return true
}

// shortCluster is `-abc` of listed no-value letters, or such letters then a value letter with its value (`-n5`).
func shortCluster(a string, flags map[string]string) bool {
	if len(a) < 3 || strings.HasPrefix(a, "--") {
		return false
	}
	for i := 1; i < len(a); i++ {
		want, ok := flags["-"+a[i:i+1]]
		if !ok {
			return false
		}
		if want != "" {
			return cliValueOK(want, a[i+1:]) // pflag reads the rest of the word as the value
		}
	}
	return true
}

func cliValueOK(want, v string) bool {
	switch {
	case v == "" || strings.HasPrefix(v, "-"):
		return false
	case want == "/":
		return strings.Count(v, "/") == 1 // owner/repo, never host/owner/repo
	}
	return want == "*" || slices.Contains(strings.Split(want, "|"), v)
}

// credentialKinds are the resources whose get/describe output carries a secret or token.
var credentialKinds = sync.OnceValue(func() []string { return shell.GuardWords("kube_credential_kinds") })

// secretKind is a kubectl resource list naming a credentialKinds kind, singular or plural, any group or case.
func secretKind(a string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == '/' }), func(k string) bool {
		k, _, _ = strings.Cut(strings.ToLower(k), ".")
		return slices.Contains(credentialKinds(), strings.TrimSuffix(k, "s"))
	})
}
