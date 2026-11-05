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

// EachGit calls fn with each git invocation in cmd and the tree it targets, until fn returns true (reported back).
// It follows substitutions, nested `bash -c`/`eval`, wrappers, `env -C`/`sudo -D`, and `cd`/`pushd` between segments.
func EachGit(cmd, cwd string, fn func(target, verb string, args []string) bool) bool {
	return eachGit(cmd, cwd, fn, 0)
}

func eachGit(cmd, cwd string, fn func(target, verb string, args []string) bool, depth int) bool {
	if depth > maxNest {
		return false
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
		rest, dir := Unwrap(toks, cwd)
		if next, ok := CdTarget(rest, cwd); ok {
			cwd = next
			continue
		}
		if segmentGit(rest, dir, fn, depth) {
			return true
		}
	}
	return false
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
