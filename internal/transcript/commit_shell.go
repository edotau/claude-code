package transcript

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/shell"
)

// gitShortCircuitOpts are git global options that print something and exit before any verb runs.
var gitShortCircuitOpts = map[string]bool{
	"--version": true, "--help": true, "-h": true, "--exec-path": true,
	"--html-path": true, "--man-path": true, "--info-path": true,
}

// maxShellRecursionDepth bounds `bash -c`, substitution and alias recursion into nested command text.
const maxShellRecursionDepth = 3

// controlWords lead a segment without being its command.
var controlWords = map[string]bool{"!": true, "if": true, "then": true, "do": true, "else": true, "elif": true,
	"while": true, "until": true}

// gitCommitCommand reports whether cmd runs `git commit` as a command word; heredoc bodies and quoted text never count.
func gitCommitCommand(cmd string) bool {
	return gitCommitCommandDepth(cmd, 0)
}

func gitCommitCommandDepth(cmd string, depth int) bool {
	if depth > maxShellRecursionDepth {
		return false
	}
	cmd = strings.ReplaceAll(stripHeredocBodies(cmd), "\\\n", "") // backslash-newline is a line continuation
	masked, subs := shell.ExtractSubstitutions(cmd)
	for _, sub := range subs {
		if gitCommitCommandDepth(sub, depth+1) {
			return true
		}
	}
	return slices.ContainsFunc(shell.Segments(masked), func(seg string) bool { return segmentCommits(seg, depth) })
}

// segmentCommits reports whether one segment runs `git commit`, directly or via a nested `bash -c`.
func segmentCommits(seg string, depth int) bool {
	toks := shell.Tokens(seg)
	for len(toks) > 0 && controlWords[toks[0]] {
		toks = toks[1:]
	}
	rest, _ := shell.Unwrap(toks, "")
	if noExecShell(rest) {
		return false
	}
	if nested, ok := shell.NestedCommand(rest); ok {
		return gitCommitCommandDepth(nested, depth+1)
	}
	return isGitCommit(rest, depth)
}

// noExecShell reports whether toks is a bash/sh/zsh call with a `-n` (noexec) flag before its `-c` string, so the
// script never runs; words after the string are positional parameters and never change that.
func noExecShell(toks []string) bool {
	if len(toks) == 0 || !slices.Contains([]string{"bash", "sh", "zsh"}, filepath.Base(toks[0])) {
		return false
	}
	for _, t := range toks[1:] {
		if len(t) < 2 || t[0] != '-' || t[1] == '-' {
			continue
		}
		if strings.ContainsRune(t, 'n') {
			return true
		}
		if strings.ContainsRune(t, 'c') {
			return false
		}
	}
	return false
}

// joinValueOpts spells `--super-prefix X`/`--config-env X` as `=` forms; internal/shell lacks them as value options.
func joinValueOpts(toks []string) []string {
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		out = append(out, toks[i])
		if (toks[i] == "--super-prefix" || toks[i] == "--config-env") && i+1 < len(toks) {
			out[len(out)-1] += "=" + toks[i+1]
			i++
		}
	}
	return out
}

// isGitCommit reports whether toks is a `git [global opts] commit` invocation (alias or literal).
func isGitCommit(toks []string, depth int) bool {
	if len(toks) == 0 || filepath.Base(toks[0]) != "git" {
		return false
	}
	toks = joinValueOpts(toks)
	_, verb, args := shell.GitInvocation(toks[1:], "")
	if verb == "" {
		return false
	}
	aliases, shortCircuit := scanGitOpts(toks[1 : len(toks)-len(args)-1])
	return !shortCircuit && (verb == "commit" || aliasIsCommit(aliases[verb], depth))
}

// scanGitOpts collects the `-c alias.<name>=<val>` pairs among git's global options and flags a short-circuit option.
func scanGitOpts(opts []string) (aliases map[string]string, shortCircuit bool) {
	aliases = map[string]string{}
	for i, o := range opts {
		if gitShortCircuitOpts[o] {
			return nil, true
		}
		if o == "-c" && i+1 < len(opts) {
			if name, val, ok := splitAliasAssign(opts[i+1]); ok && !gitBuiltinVerbs[name] {
				aliases[name] = val // last -c alias.<name>=<val> for a given name wins
			}
		}
	}
	return aliases, false
}

// gitBuiltinVerbs are git's own command names — git ignores a user alias defined under one of these
// names, so `-c alias.<builtin>=…` never registers (the builtin behavior still runs unaltered).
var gitBuiltinVerbs = map[string]bool{
	"status": true, "add": true, "log": true, "diff": true, "push": true, "pull": true, "fetch": true,
	"checkout": true, "switch": true, "branch": true, "merge": true, "rebase": true, "reset": true,
	"show": true, "tag": true, "stash": true, "clone": true, "init": true, "rm": true, "mv": true,
	"restore": true, "commit": true,
}

// splitAliasAssign splits `alias.<name>=<value>` (from `git -c <val>`) into name and raw value.
func splitAliasAssign(val string) (name, value string, ok bool) {
	n, v, hasEq := strings.Cut(val, "=")
	if !hasEq {
		return "", "", false
	}
	const prefix = "alias."
	if !strings.HasPrefix(n, prefix) {
		return "", "", false
	}
	return strings.TrimPrefix(n, prefix), v, true
}

// aliasIsCommit reports whether an alias's raw value runs a commit: either directly (`commit`) or as a
// shell alias (`!git commit …`, recursed depth-limited); val == "" means no alias was registered.
func aliasIsCommit(val string, depth int) bool {
	if val == "" {
		return false
	}
	if val == "commit" {
		return true
	}
	if rest, ok := strings.CutPrefix(val, "!"); ok {
		return depth < maxShellRecursionDepth && gitCommitCommandDepth(rest, depth+1)
	}
	return false
}

// stripHeredocBodies removes the body lines of every `<<WORD`/`<<'WORD'`/`<<-WORD` heredoc (and its
// terminator line) from cmd, so a commit mentioned only inside a heredoc body never counts as run.
func stripHeredocBodies(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		word, ok := heredocWord(lines[i])
		if !ok {
			continue
		}
		i++
		for i < len(lines) && strings.TrimSpace(lines[i]) != word {
			i++
		}
		// i now sits on the terminator line (or past EOF); either way it is dropped, not re-appended.
	}
	return strings.Join(out, "\n")
}

var bareHeredocWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// heredocWord reports the terminator word of a `<<[-]WORD` (quoted or bare) on line, false for a
// here-string (`<<<`) or a line with no heredoc redirect at all.
func heredocWord(line string) (string, bool) {
	idx := strings.Index(line, "<<")
	if idx < 0 || (idx+2 < len(line) && line[idx+2] == '<') {
		return "", false
	}
	rest := strings.TrimLeft(strings.TrimPrefix(line[idx+2:], "-"), " \t")
	if rest == "" {
		return "", false
	}
	if rest[0] == '\'' || rest[0] == '"' {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", false
		}
		return rest[1 : 1+end], rest[1:1+end] != ""
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	word := rest[:end]
	return word, bareHeredocWord.MatchString(word) // rejects `$((1<<4))` and `"a <<b"`
}
