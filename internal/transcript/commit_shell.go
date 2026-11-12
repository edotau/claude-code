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

// gitBuiltinVerbs are git's builtin command names (`git --list-cmds=builtins`, git 2.53): git never runs
// an alias under one of these names, so `-c alias.<builtin>=…` never registers.
var gitBuiltinVerbs = map[string]bool{
	"add": true, "am": true, "annotate": true, "apply": true, "archive": true, "backfill": true,
	"bisect": true, "blame": true, "branch": true, "bugreport": true, "bundle": true, "cat-file": true,
	"check-attr": true, "check-ignore": true, "check-mailmap": true, "check-ref-format": true,
	"checkout": true, "checkout-index": true, "cherry": true, "cherry-pick": true, "clean": true,
	"clone": true, "column": true, "commit": true, "commit-graph": true, "commit-tree": true,
	"config": true, "count-objects": true, "credential": true, "credential-cache": true,
	"credential-store": true, "describe": true, "diagnose": true, "diff": true, "diff-files": true,
	"diff-index": true, "diff-pairs": true, "diff-tree": true, "difftool": true, "fast-export": true,
	"fast-import": true, "fetch": true, "fetch-pack": true, "fmt-merge-msg": true, "for-each-ref": true,
	"for-each-repo": true, "format-patch": true, "fsck": true, "fsck-objects": true, "fsmonitor--daemon": true,
	"gc": true, "get-tar-commit-id": true, "grep": true, "hash-object": true, "help": true, "hook": true,
	"index-pack": true, "init": true, "init-db": true, "interpret-trailers": true, "last-modified": true,
	"log": true, "ls-files": true, "ls-remote": true, "ls-tree": true, "mailinfo": true, "mailsplit": true,
	"maintenance": true, "merge": true, "merge-base": true, "merge-file": true, "merge-index": true,
	"merge-ours": true, "merge-recursive": true, "merge-subtree": true, "merge-tree": true, "mktag": true,
	"mktree": true, "multi-pack-index": true, "mv": true, "name-rev": true, "notes": true,
	"pack-objects": true, "pack-redundant": true, "pack-refs": true, "patch-id": true, "pickaxe": true,
	"prune": true, "prune-packed": true, "pull": true, "push": true, "range-diff": true, "read-tree": true,
	"rebase": true, "receive-pack": true, "reflog": true, "refs": true, "remote": true, "repack": true,
	"replace": true, "replay": true, "repo": true, "rerere": true, "reset": true, "restore": true,
	"rev-list": true, "rev-parse": true, "revert": true, "rm": true, "send-pack": true, "shortlog": true,
	"show": true, "show-branch": true, "show-index": true, "show-ref": true, "sparse-checkout": true,
	"stage": true, "stash": true, "status": true, "stripspace": true, "submodule": true, "switch": true,
	"symbolic-ref": true, "tag": true, "unpack-file": true, "unpack-objects": true, "update-index": true,
	"update-ref": true, "update-server-info": true, "upload-archive": true, "upload-pack": true, "var": true,
	"verify-commit": true, "verify-pack": true, "verify-tag": true, "version": true, "whatchanged": true,
	"worktree": true, "write-tree": true,
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
	if f := strings.Fields(val); len(f) > 0 && f[0] == "commit" {
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
		word, stripTabs, ok := heredocWord(lines[i])
		if !ok {
			continue
		}
		i++
		for i < len(lines) && !isHeredocEnd(lines[i], word, stripTabs) {
			i++
		}
		// i now sits on the terminator line (or past EOF); either way it is dropped, not re-appended.
	}
	return strings.Join(out, "\n")
}

// isHeredocEnd reports whether line closes a heredoc: an exact match, with leading tabs stripped only for `<<-`.
func isHeredocEnd(line, word string, stripTabs bool) bool {
	if stripTabs {
		line = strings.TrimLeft(line, "\t")
	}
	return line == word
}

var bareHeredocWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// heredocWord reports the terminator word of a `<<[-]WORD` (quoted or bare) on line and whether it is `<<-`;
// ok is false for a here-string (`<<<`) or a line with no heredoc redirect at all.
func heredocWord(line string) (word string, stripTabs, ok bool) {
	idx := unquotedHeredocOp(line)
	if idx < 0 || (idx+2 < len(line) && line[idx+2] == '<') {
		return "", false, false
	}
	stripTabs = strings.HasPrefix(line[idx+2:], "-")
	rest := strings.TrimLeft(strings.TrimPrefix(line[idx+2:], "-"), " \t")
	if rest == "" {
		return "", false, false
	}
	if rest[0] == '\'' || rest[0] == '"' {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return "", false, false
		}
		return rest[1 : 1+end], stripTabs, rest[1:1+end] != ""
	}
	end := strings.IndexAny(rest, " \t;|&<>")
	if end < 0 {
		end = len(rest)
	}
	word = rest[:end]
	return word, stripTabs, bareHeredocWord.MatchString(word) // rejects `$((1<<4))` and `"a <<b"`
}

// unquotedHeredocOp returns the index of the first `<<` on line outside quotes and backslash escapes, or -1.
func unquotedHeredocOp(line string) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '<' && strings.HasPrefix(line[i:], "<<"):
			return i
		}
	}
	return -1
}
