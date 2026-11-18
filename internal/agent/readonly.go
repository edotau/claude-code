package agent

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

// ReadOnlyBashHeads are the command prefixes of the list, for an agent whose native matcher takes "<head> *" patterns.
func ReadOnlyBashHeads() []string {
	heads := []string{"rg", "grep", "ls", "wc", "head", "tail", "cat", "sed -n", "go doc", "go list", "go env", "go version",
		"git branch --list"}
	for _, v := range gitPlainVerbs {
		if !slices.Contains(gitGatedVerbs, v) {
			heads = append(heads, "git "+v)
		}
	}
	for _, group := range slices.Sorted(maps.Keys(readOnlyClaudeVerbs)) {
		for _, v := range readOnlyClaudeVerbs[group] {
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
	if len(toks) == 2 && helpWord().MatchString(toks[0]) && slices.Contains([]string{"--help", "-h", "--version"}, toks[1]) ||
		claudeHelp(toks) {
		return true
	}
	return readOnlyCommand(toks)
}

// isEnvWord is the `env` wrapper, which prints the environment (credentials) or runs a command: refused in every form.
func isEnvWord(t string) bool { return filepath.Base(t) == "env" }

// rtkPlain is the plain command `rtk <sub> …` stands for; claude-code is no rtk subcommand, so rtk runs it as typed.
func rtkPlain(toks []string) []string {
	if len(toks) < 2 || toks[0] != "rtk" {
		return toks
	}
	switch toks[1] {
	case "git", "ls", "grep", "rg", "find", "claude-code":
		return toks[1:]
	case "read":
		return append([]string{"cat"}, toks[2:]...)
	}
	return toks
}

// claudeHelp is `claude-code <verb> [<subverb>] -h|--help`: help text never writes.
func claudeHelp(toks []string) bool {
	n := len(toks)
	if n < 3 || n > 4 || toks[0] != "claude-code" || toks[n-1] != "-h" && toks[n-1] != "--help" {
		return false
	}
	return !slices.ContainsFunc(toks[1:n-1], func(t string) bool { return !helpWord().MatchString(t) })
}

// rawSegmentOK is the as-typed part of the check: no write or `<`, and only a harmless env or time prefix.
func rawSegmentOK(seg string, rawToks, toks []string) bool {
	if hasWrite(seg) {
		return false
	}
	n := len(rawToks) - len(toks)
	if n == 0 {
		return true
	}
	if !grantsNormalized(rawToks, toks) {
		return false
	}
	return !envPrefixRisk(rawToks[:n], seg)
}

// envPrefixRisk is a prefix assignment that could steer git or a pager, or any `$` a prefix assignment could feed.
func envPrefixRisk(prefix []string, seg string) bool {
	for _, t := range prefix {
		m := envAssign().FindStringSubmatch(t)
		if m == nil {
			continue
		}
		up := strings.ToUpper(m[1])
		if strings.HasPrefix(up, "GIT_") || strings.Contains(up, "PAGER") || strings.Contains(seg, "$") {
			return true
		}
	}
	return false
}

// readOnlyCommand judges a wrapper-free command by its name.
func readOnlyCommand(toks []string) bool {
	switch toks[0] {
	case "git":
		return readOnlyGit(toks)
	case "claude-code":
		return readOnlyClaudeCode(toks[1:])
	case "go":
		return readOnlyGo(toks[1:])
	case "rg":
		return noFlag(toks[1:], "--pre", "--pre-glob", "-z", "--search-zip", "--hostname-bin")
	case "grep", "ls", "wc", "cat", "echo", "printf":
		return true
	case "find":
		return noFlag(toks[1:], findActions...)
	case "head", "tail":
		return noFlag(toks[1:], "-f", "-F", "--follow", "--retry")
	case "sed":
		return sedPrintMatches(strings.Join(toks, " "))
	}
	return readOnlyCLI(toks)
}

// findActions are the find tokens that write a file or run a command.
var findActions = []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"}

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
var gitPlainVerbs = []string{"log", "show", "diff", "status", "blame", "ls-files", "rev-parse", "grep", "branch",
	"remote", "worktree", "stash", "clean", "reflog"}

// gitGatedVerbs are the gitPlainVerbs that also write, so only the read forms their arg check names pass.
var gitGatedVerbs = []string{"branch", "remote", "worktree", "stash", "clean", "reflog"}

// gitBannedLong lets the caller pick a program, config, repo or output file (abbreviations too); not shell.ReadOnlyGit.
var gitBannedLong = []string{"--output", "--exec-path", "--open-files-in-pager", "--config-env", "--config", "--git-dir",
	"--work-tree", "--paginate", "--pager", "--contents", "--ext-diff", "--textconv"}

// readOnlyGit is plain git: configured drivers are the owner's; the caller picks no program, config, repo or output.
func readOnlyGit(toks []string) bool {
	rest, dirs := gitGlobalOpts(toks[1:])
	if len(rest) == 0 || !slices.Contains(gitPlainVerbs, rest[0]) || !gitArgsOK(rest[1:]) || len(dirs) > 0 && noIndexDiff(rest) {
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

func gitArgsOK(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch {
		case a == "-c" || a == "-C" || strings.HasPrefix(a, "-O"):
			return false
		case strings.HasPrefix(a, "--"):
			if slices.ContainsFunc(gitBannedLong, func(b string) bool { return len(name) >= 4 && name != "--text" && strings.HasPrefix(b, name) }) {
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

var lsFilesFlags = []string{"-d", "-m", "-o", "-i", "-s", "-u", "-k", "-z", "-t", "-v", "-f", "--cached", "--deleted",
	"--modified", "--others", "--ignored", "--stage", "--unmerged", "--killed", "--directory", "--no-empty-directory",
	"--eol", "--full-name", "--exclude-standard", "--deduplicate", "--error-unmatch", "--"}

func lsFilesArgs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !slices.Contains(lsFilesFlags, a) {
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
		case slices.Contains([]string{"--show-current", "-a", "--all", "-r", "--remotes", "-v", "-vv"}, a):
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
		return !slices.Contains([]string{"-v", "--verbose", "--porcelain", "-z"}, a)
	})
}

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
var writeFlags = []string{"--out", "--output", "-o", "--write", "--fix", "--apply", "--save", "--delete", "--remove",
	"--kill", "--reap", "--clean", "--prune", "--force", "--sync", "--set", "--install", "--restore"}

var readOnlyClaudeVerbs = map[string][]string{
	"agents":  {"find", "diff", "result", "wait", "status", "tree", "list", "label"},
	"memory":  {"recall", "search", "show", "path"},
	"review":  {"simplify", "scope", "quality"},
	"plan":    {"status", "assumptions", "goals"},
	"models":  {"preflight"},
	"network": {"websearch"},
	"gateway": {"resolve"},
	"vault":   {"get", "ls"},
}

// readOnlyClaudeCode is a listed `claude-code <group> <verb>` with no write-looking flag; `agents diff --check` is out.
func readOnlyClaudeCode(args []string) bool {
	if len(args) < 2 || !slices.Contains(readOnlyClaudeVerbs[args[0]], args[1]) {
		return false
	}
	switch args[0] + " " + args[1] {
	case "review simplify":
		return simplifyArgsOK(args[2:])
	case "review scope":
		return scopeArgsOK(args[2:])
	case "models preflight":
		return noFlag(args[2:], "--refresh", "--discover", "--live", "--profile")
	case "vault get":
		return noFlag(args[2:], append(slices.Clone(writeFlags), "--key")...)
	case "agents diff":
		return noFlag(args[2:], append(slices.Clone(writeFlags), "--check")...)
	}
	return noFlag(args[2:], writeFlags...)
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
		return noFlag(args[1:], "-w", "-u", "-changed")
	case "list":
		return noFlag(args[1:], "-toolexec", "-exec", "-export", "-compiled", "-mod", "-modfile", "-overlay", "-pkgdir", "-u", "-test")
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
	if len(toks) > 0 && slices.Contains(noFileCommands, toks[0]) {
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
var noFileCommands = []string{"echo", "printf", "true", ":", "pwd"}

// cdReadDenied refuses a cd that is not one literal absolute path, or whose target a Read deny covers or sits above.
func cdReadDenied(reads []Rule, toks []string) bool {
	if len(toks) != 2 || !path.IsAbs(filepath.ToSlash(toks[1])) || strings.ContainsAny(toks[1], "$`*?[{~\\") {
		return true
	}
	target := path.Clean(toks[1])
	targets := []string{target, paths.CanonDeep(target)}
	return slices.ContainsFunc(reads, func(r Rule) bool {
		dirs := append(denyParentDirs(r.Arg), denyParentDirs(expandHome("Read", r.Arg))...)
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
var patchFlags = []string{"-p", "-u", "-L", "--patch", "--cc", "--dd", "--patch-with-stat", "--patch-with-raw"}

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
	_, cli := readOnlyCLIs[name]
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
			return slices.Contains(patchFlags, a) || strings.HasPrefix(a, "-L") || strings.HasPrefix(a, "--patch")
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

// cliForm is a read-only subcommand (one or two words) of a cluster or cloud CLI and the flags it must not carry.
type cliForm struct {
	words  string
	banned []string
}

func cliForms(banned []string, words ...string) []cliForm {
	forms := make([]cliForm, len(words))
	for i, w := range words {
		forms[i] = cliForm{w, banned}
	}
	return forms
}

// kubeBanned are flags that pick another cluster or identity, read a raw API path, or never end.
var kubeBanned = []string{"--raw", "--token", "--kubeconfig", "--server", "-s", "--as", "--as-group", "--as-uid", "--username",
	"--password", "--client-key", "--client-certificate", "--certificate-authority", "-f", "--follow", "-w", "--watch", "--watch-only"}

var kubeForms = cliForms(kubeBanned, "get", "describe", "logs", "top", "version", "api-resources", "explain",
	"config get-contexts", "config current-context")

// readOnlyCLIs is the (tool, subcommand) table: nothing is allowed by tool name alone; kubectl config view is out.
var readOnlyCLIs = map[string][]cliForm{
	"kubectl": kubeForms,
	"oc":      kubeForms,
	"docker": append(cliForms([]string{"--follow"}, "ps", "images", "inspect", "version", "info"),
		cliForm{"logs", []string{"--follow", "-f"}}),
	"gh": cliForms([]string{"--web", "-w", "--watch", "--show-token", "-t"}, "pr view", "pr list", "pr status", "pr diff",
		"pr checks", "issue view", "issue list", "issue status", "repo view", "run view", "run list", "release view",
		"release list", "auth status"),
	"aws":    cliForms(nil, "--version", "help", "sts get-caller-identity"),
	"gcloud": cliForms(nil, "--version", "version", "help", "config list"),
}

// cliGlobals are the value flags a tool may carry before its subcommand; any other leading flag refuses the command.
var cliGlobals = map[string][]string{
	"kubectl": {"-n", "--namespace", "--context"}, "oc": {"-n", "--namespace", "--context"}, "docker": {"--context"},
}

// readOnlyCLI judges `<tool> [globals] <subcommand> …` against readOnlyCLIs.
func readOnlyCLI(toks []string) bool {
	forms, ok := readOnlyCLIs[toks[0]]
	if !ok {
		return false
	}
	rest, ok := skipCLIGlobals(toks[0], toks[1:])
	if !ok || slices.ContainsFunc(rest, secretKind) && (toks[0] == "kubectl" || toks[0] == "oc") {
		return false
	}
	return slices.ContainsFunc(forms, func(f cliForm) bool {
		w := strings.Fields(f.words)
		return len(rest) >= len(w) && slices.Equal(rest[:len(w)], w) && !slices.ContainsFunc(rest[len(w):], func(a string) bool {
			return slices.ContainsFunc(f.banned, func(b string) bool { return bannedCLIFlag(a, b) })
		})
	})
}

// skipCLIGlobals drops the leading value flags the tool allows (`-n ns`, `--context=x`); a bad value is not ok.
func skipCLIGlobals(tool string, rest []string) ([]string, bool) {
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		name, val, hasVal := strings.Cut(rest[0], "=")
		if !slices.Contains(cliGlobals[tool], name) {
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

// bannedCLIFlag: a long flag matches by name (any =value), a short one also with an attached value (-sURL).
func bannedCLIFlag(a, banned string) bool {
	name, _, _ := strings.Cut(a, "=")
	return name == banned || len(banned) == 2 && strings.HasPrefix(a, banned) && !strings.HasPrefix(a, "--")
}

// secretKind is a kubectl resource list naming secrets, whose get/describe output carries credentials.
func secretKind(a string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == '/' }), func(k string) bool {
		k, _, _ = strings.Cut(strings.ToLower(k), ".")
		return k == "secret" || k == "secrets"
	})
}
