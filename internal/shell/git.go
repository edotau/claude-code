package shell

import (
	"path/filepath"
	"strings"
)

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

// GitInvocation parses git's global options up to the verb: the target tree, the verb, the verb's words.
func GitInvocation(toks []string, cwd string) (target, verb string, args []string) {
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
		}
	}
	if i < len(toks) {
		verb, args = toks[i], toks[i+1:]
	}
	return gitTarget(cwd, gitDir, workTree, cPaths), verb, args
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
	if p, ok := ResolvePath(workTree, cwd); workTree != "" && ok {
		return p
	}
	if p, ok := ResolvePath(gitDir, cwd); gitDir != "" && ok && filepath.Base(p) == ".git" {
		return filepath.Dir(p)
	}
	target := cwd
	for _, c := range cPaths { // successive -C paths apply relative to each other
		if p, ok := ResolvePath(c, target); ok {
			target = p
		}
	}
	return target
}

// GitCommitInvocation reports whether cmd, or a nested/substituted command within it, invokes `git commit`;
// nesting too deep to inspect counts as a possible commit.
func GitCommitInvocation(cmd string) bool {
	return EachGit(cmd, "", func(_, verb string, _ []string) bool { return verb == "commit" || verb == Uninspectable })
}
