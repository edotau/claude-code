package agent

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
		if v != "branch" {
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

var helpWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// readOnlySegment judges one segment; raw is the as-typed spelling (quotes intact), else the quote-stripped one.
func readOnlySegment(seg string, raw bool) bool {
	rawToks := shell.Tokens(seg)
	toks, _ := shell.Unwrap(rawToks, "")
	if len(toks) == 0 || raw && !rawSegmentOK(seg, rawToks, toks) {
		return false
	}
	if len(toks) == 2 && helpWord.MatchString(toks[0]) && slices.Contains([]string{"--help", "-h", "--version"}, toks[1]) {
		return true
	}
	return readOnlyCommand(toks)
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
		m := envAssign.FindStringSubmatch(t)
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
	case "grep", "ls", "wc", "cat":
		return true
	case "head", "tail":
		return noFlag(toks[1:], "-f", "-F", "--follow", "--retry")
	case "sed":
		return sedPrintMatches(strings.Join(toks, " "))
	}
	return false
}

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
var gitPlainVerbs = []string{"log", "show", "diff", "status", "blame", "ls-files", "rev-parse", "grep", "branch"}

// gitBannedLong lets the caller pick a program, config, repo or output file (abbreviations too); not shell.ReadOnlyGit.
var gitBannedLong = []string{"--output", "--exec-path", "--open-files-in-pager", "--config-env", "--config", "--git-dir",
	"--work-tree", "--paginate", "--pager", "--contents"}

// readOnlyGit is plain git: configured drivers are the owner's; the caller picks no program, config, repo or output.
func readOnlyGit(toks []string) bool {
	rest := toks[1:]
	for len(rest) > 0 && (rest[0] == "--no-pager" || rest[0] == "--no-optional-locks") {
		rest = rest[1:]
	}
	if len(rest) == 0 || !slices.Contains(gitPlainVerbs, rest[0]) || !gitArgsOK(rest[1:]) {
		return false
	}
	switch rest[0] {
	case "ls-files":
		return lsFilesArgs(rest[1:])
	case "branch":
		return branchArgs(rest[1:])
	}
	return true
}

func gitArgsOK(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		switch {
		case a == "-c" || a == "-C" || strings.HasPrefix(a, "-O"):
			return false
		case strings.HasPrefix(a, "--"):
			if slices.ContainsFunc(gitBannedLong, func(b string) bool { return len(name) >= 4 && strings.HasPrefix(b, name) }) {
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

// branchArgs is `git branch` only as a listing: --list/-l/--show-current plus read-only listing flags.
func branchArgs(args []string) bool {
	list := false
	for _, a := range args {
		switch {
		case a == "--list" || a == "-l" || a == "--show-current":
			list = true
		case strings.HasPrefix(a, "-") && !slices.Contains([]string{"-a", "--all", "-r", "--remotes", "-v", "-vv"}, a):
			return false
		}
	}
	return list
}

// writeFlags are option names that suggest an output file, a repair or a deletion; any one denies a claude-code verb.
var writeFlags = []string{"--out", "--output", "-o", "--write", "--fix", "--apply", "--save", "--delete", "--remove",
	"--kill", "--reap", "--clean", "--prune", "--force", "--sync", "--set", "--install", "--restore"}

var readOnlyClaudeVerbs = map[string][]string{
	"agents":  {"find", "diff", "result", "status", "tree", "list", "label"},
	"memory":  {"recall", "search", "show", "path", "timeline"},
	"review":  {"simplify", "assumptions", "goals"},
	"network": {"websearch"},
}

// readOnlyClaudeCode is a listed `claude-code <group> <verb>` without a write-looking flag; `agents diff --check` is out.
func readOnlyClaudeCode(args []string) bool {
	if len(args) < 2 || !slices.Contains(readOnlyClaudeVerbs[args[0]], args[1]) {
		return false
	}
	switch args[0] + " " + args[1] {
	case "review simplify":
		return simplifyArgsOK(args[2:])
	case "agents diff":
		return noFlag(args[2:], append(slices.Clone(writeFlags), "--check")...)
	}
	return noFlag(args[2:], writeFlags...)
}

// unsafeSimplifyRune is any rune outside letters, digits and the plain punctuation a path or ref needs.
func unsafeSimplifyRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,-", r)
}

// simplifyArgsOK allows only the detector's --json, --threshold <level> and --base <ref> flags plus local paths.
func simplifyArgsOK(args []string) bool {
	valueOK := map[string]func(string) bool{
		"--threshold": func(v string) bool { return slices.Contains([]string{"strict", "medium", "relaxed"}, v) },
		"--base":      func(v string) bool { return !strings.HasPrefix(v, "-") },
	}
	for ; len(args) > 0; args = args[1:] {
		a := args[0]
		if strings.ContainsFunc(a, unsafeSimplifyRune) {
			return false
		}
		if ok, isValued := valueOK[a]; isValued {
			if len(args) < 2 || !ok(args[1]) || strings.ContainsFunc(args[1], unsafeSimplifyRune) {
				return false
			}
			args = args[1:]
		} else if a != "--json" && (strings.HasPrefix(a, "-") || !filepath.IsLocal(a)) {
			return false
		}
	}
	return true
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

// readOnlyReadDenied: a Read deny exists and the segment's operand matches it or its operands cannot be listed.
func readOnlyReadDenied(p Permissions, seg string) bool {
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
	ops, ok := readOperands(toks)
	if !ok || slices.ContainsFunc(reads, func(r Rule) bool { return r.Arg == "" }) {
		return true
	}
	deny := Permissions{Deny: reads}
	return slices.ContainsFunc(ops, func(o string) bool {
		return slices.ContainsFunc(operandForms(o), func(f string) bool { return deny.Decide("Read", f) == Deny })
	})
}

// operandForms is an operand as typed, cleaned, absolute from the cwd, and every trailing sub-path of it.
func operandForms(o string) []string {
	c := path.Clean(o)
	forms := []string{o, c}
	if !path.IsAbs(c) {
		if wd, err := os.Getwd(); err == nil {
			forms = append(forms, path.Join(filepath.ToSlash(wd), c))
		}
	}
	parts := strings.Split(c, "/")
	for i := 1; i < len(parts); i++ {
		forms = append(forms, strings.Join(parts[i:], "/"))
	}
	return forms
}

// patchFlags make git log print file contents.
var patchFlags = []string{"-p", "-u", "-L", "--patch", "--cc", "--dd", "--patch-with-stat", "--patch-with-raw"}

// readOperands are the file operands of a read-only command; ok is false when they cannot be listed reliably.
func readOperands(toks []string) (ops []string, ok bool) {
	if len(toks) == 0 {
		return nil, false
	}
	args := toks[1:]
	files := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return strings.HasPrefix(a, "-") })
	if slices.ContainsFunc(files, func(f string) bool { return strings.ContainsAny(f, "*?[{$~") }) {
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
	return nil, false // rg searches the tree
}

func recursiveGrep(a string) bool {
	return a == "--recursive" || a == "--dereference-recursive" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
		strings.ContainsAny(a, "rR")
}

// gitReadOperands: status, rev-parse, ls-files, branch and a patch-less log show no contents; the rest cannot be mapped.
func gitReadOperands(args []string) ([]string, bool) {
	for len(args) > 0 && strings.HasPrefix(args[0], "--no-") {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "status", "rev-parse", "ls-files", "branch":
		return nil, true
	case "log":
		return nil, !slices.ContainsFunc(args[1:], func(a string) bool {
			return slices.Contains(patchFlags, a) || strings.HasPrefix(a, "-L") || strings.HasPrefix(a, "--patch")
		})
	}
	return nil, false
}

// claudeReadOperands: agents find|diff read the tree, review simplify its files; the other verbs read no repo path.
func claudeReadOperands(args, files []string) ([]string, bool) {
	if len(args) < 2 {
		return nil, false
	}
	switch args[0] + " " + args[1] {
	case "agents find", "agents diff":
		return nil, false
	case "review simplify":
		return files[2:], true
	}
	return nil, true
}
