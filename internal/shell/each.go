package shell

import (
	"path/filepath"
	"slices"
	"strings"
)

// maxNest bounds substitution/`bash -c`/`eval` recursion so a pathological command cannot loop.
const maxNest = 4

// shellKeywords lead a segment without being its command (`if cd x`, `! git`, `then git`).
var shellKeywords = []string{"if", "then", "else", "elif", "do", "while", "until", "!", "{", "("}

// Uninspectable is the verb EachGit reports, with the cwd as target, for nesting deeper than maxNest: the callee cannot
// see what runs there, so a guard must fail closed on it.
const Uninspectable = "<uninspectable>"

// EachGit calls fn with each git invocation in cmd and the tree it targets, until fn returns true (reported back).
// It follows substitutions, nested `bash -c`/`eval`, wrappers, `env -C`/`sudo -D`, and `cd`/`pushd` between segments.
// Nesting past maxNest reports one Uninspectable call instead of being skipped.
func EachGit(cmd, cwd string, fn func(target, verb string, args []string) bool) bool {
	return eachGit(cmd, cwd, fn, 0)
}

func eachGit(cmd, cwd string, fn func(target, verb string, args []string) bool, depth int) bool {
	if depth > maxNest {
		return fn(cwd, Uninspectable, nil)
	}
	masked, subs := ExtractSubstitutions(cmd)
	for _, sub := range subs { // a substitution runs nested: its cd stays inside it
		if eachGit(sub, cwd, fn, depth+1) {
			return true
		}
	}
	for _, seg := range Segments(masked) {
		toks := commandWords(seg)
		if len(toks) == 0 {
			continue
		}
		if next, ok := CdTarget(builtinPrefixDropped(toks), cwd); ok { // only a builtin cd moves this shell
			cwd = next
			continue
		}
		rest, dir := Unwrap(toks, cwd)
		if segmentGit(rest, dir, fn, depth) {
			return true
		}
	}
	return false
}

// builtinPrefixDropped drops what still runs the next word in the current shell (NAME=val, `command`, `builtin`, `time`),
// unlike env/sudo/nohup/nice/timeout/exec, which start another process: a `cd` behind those cannot move this shell.
func builtinPrefixDropped(toks []string) []string {
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 || toks[0] != "command" && toks[0] != "builtin" && toks[0] != "time" {
			return toks
		}
		toks, _ = skipWrapperOpts(toks[0], toks[1:], "")
	}
}

// commandWords is the segment's words without leading shell keywords; nil for an ssh command (it runs remotely).
func commandWords(seg string) []string {
	toks := Tokens(seg)
	for len(toks) > 0 && slices.Contains(shellKeywords, toks[0]) {
		toks = toks[1:]
	}
	if len(toks) > 0 && toks[0] == "ssh" {
		return nil
	}
	return toks
}

// segmentGit visits the nested command and each git word of one segment whose wrappers are already dropped.
func segmentGit(toks []string, dir string, fn func(target, verb string, args []string) bool, depth int) bool {
	if nested, ok := nestedOf(toks); ok { // its words are the nested command's, visited once there
		return eachGit(nested, dir, fn, depth+1)
	}
	for i, t := range toks { // any word: xargs/watch/find -exec need no modelling
		if (t == "git" || strings.HasSuffix(t, "/git")) && filepath.Base(t) == "git" && fn(GitInvocation(toks[i+1:], dir)) {
			return true
		}
	}
	return false
}
