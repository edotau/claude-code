// Read-only git allowlist: judges the shell commands a --read-only run may execute.

package shell

import (
	"slices"
	"strings"
)

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

// badGitArg is -c, a ~ path, a banned flag, or a dash arg outside the verb's allowed set (when it has one).
func badGitArg(a string, allowed map[string]bool) bool {
	return a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) || allowed != nil && strings.HasPrefix(a, "-") && !allowed[a]
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
