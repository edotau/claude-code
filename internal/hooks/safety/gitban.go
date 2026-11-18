package safety

import (
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = sync.OnceValue(func() map[string]bool { return shell.GuardSet("git_shared_tree_banned_verbs") })

// harnessGitVerb is the first banned verb, or shell.Uninspectable, whose target tree is under the config dir.
func harnessGitVerb(cmd, cwd string) string {
	cmd = expandVars(dropRemoteHeredocs(core.DropDataHeredocs(cmd))) // `T=/tmp/x; git -C $T`
	hit := treeVerb(cmd, cwd)
	if hit != shell.Uninspectable {
		return hit
	}
	// A printf/echo fed to a shell that decodes to text running no git is not too deep to check.
	outer, texts := shellFeeds(cmd)
	if len(texts) == 0 || bannedVerb(outer, cwd) != "" || slices.ContainsFunc(texts, runsGit) {
		return hit
	}
	return ""
}

var gitDirOpt = regexp.MustCompile(`(--git-dir(=|\s+)|GIT_DIR=)("[^"]*"|'[^']*'|[^\s;&|)]+)`)

var workTreeOpt = regexp.MustCompile(`(--work-tree(=|\s+)|GIT_WORK_TREE=)("[^"]*"|'[^']*'|[^\s;&|)]+)`)

// treeVerb is bannedVerb judged on the git dir and the work tree: with none given git's work tree is cwd.
func treeVerb(cmd, cwd string) string {
	if !gitEnvAssign.MayMatch(cmd) && !strings.Contains(cmd, "--git-dir") && !strings.Contains(cmd, "--work-tree") {
		return bannedVerb(cmd, cwd)
	}
	variants := []string{withGitEnv(cmd)}
	if !workTreeOpt.MatchString(cmd) {
		variants = append(variants, gitDirOpt.ReplaceAllString(cmd, ""))
	} else if m := gitDirOpt.FindStringSubmatch(cmd); m != nil && path.Base(strings.Trim(m[3], `"'`)) == ".git" {
		variants = append(variants, withGitEnv(workTreeOpt.ReplaceAllString(cmd, "")))
	}
	for _, v := range variants {
		if hit := bannedVerb(v, cwd); hit != "" {
			return hit
		}
	}
	return ""
}

var gitEnvAssign = core.Gate(`(?:^|[\s;&|(])(GIT_DIR|GIT_WORK_TREE)=("[^"]*"|'[^']*'|[^\s;&|)]+)`, "GIT_DIR=", "GIT_WORK_TREE=")

var gitWord = regexp.MustCompile(`(^|[\s;&|('"])((?:\S*/)?git)(\s)`)

// withGitEnv spells GIT_DIR=/GIT_WORK_TREE= as --git-dir/--work-tree on each git word, so one resolver reads both.
func withGitEnv(cmd string) string {
	if !gitEnvAssign.MayMatch(cmd) {
		return cmd
	}
	var opts strings.Builder
	for _, m := range gitEnvAssign.Regexp().FindAllStringSubmatch(cmd, -1) {
		flag := " --git-dir="
		if m[1] == "GIT_WORK_TREE" {
			flag = " --work-tree="
		}
		opts.WriteString(flag)
		opts.WriteString(m[2])
	}
	return gitWord.ReplaceAllString(cmd, "${1}${2}"+strings.ReplaceAll(opts.String(), "$", "$$")+"${3}")
}

// bannedVerb is harnessGitVerb's first hit in cmd, with no feed decoding.
func bannedVerb(cmd, cwd string) string {
	var hit string
	fresh := mktempCdsOnly(cmd)
	shell.EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if shell.UnknownDir(target) && !fresh { // a dynamic cd may land back in the shared tree
			target = cwd
		}
		if !gitBannedUnder(target, verb, args) || tooDeepNoVerb(verb, args, cmd) {
			return false
		}
		hit = verb
		return true
	})
	return hit
}

// mktempCd: a cd/pushd's target word, a shell assignment, and a target that is a fresh `$(mktemp …)` dir or a var.
var mktempCd = []*core.Gated{
	core.Gate(`(?:^|[\s;&|(])(?:cd|pushd)\s+(?:-[LPe@]+\s+)*(?:--\s+)?("[^"]*"|\$\([^)]*\)\S*|\S+)`, "cd", "pushd"),
	core.Gate(`(?:^|[\s;&|(])([A-Za-z_]\w*)=`, "="),
	core.Gate(`^"?\$\(\s*mktemp(?:\s+[-\w./=]+)*\s*\)"?(?:[\s;&|)]|$)`, "mktemp"),
	core.Gate(`^(?:\$\(\s*mktemp(?:\s+[-\w./=]+)*\s*\)|\$\{?([A-Za-z_]\w*)\}?)(?:/[-\w./]*)?$`, "$"),
}

// mktempCdsOnly reports a command whose every dynamic cd lands in a fresh mktemp dir, never the shared tree.
func mktempCdsOnly(cmd string) bool {
	re := mktempCd
	if !re[0].MayMatch(cmd) { // no cd at all, so fresh stays false
		return false
	}
	vars := map[string]bool{} // false once any assignment is not a mktemp dir
	for _, m := range re[1].Regexp().FindAllStringSubmatchIndex(cmd, -1) {
		name := cmd[m[2]:m[3]]
		if prev, seen := vars[name]; !seen || prev {
			vars[name] = re[2].MatchString(cmd[m[1]:]) && freshMktemp(cmd[m[1]:])
		}
	}
	fresh := false
	for _, m := range re[0].Regexp().FindAllStringSubmatch(cmd, -1) {
		arg := strings.ReplaceAll(m[1], `"`, "")
		if !strings.ContainsAny(arg, "$`*?") {
			continue
		}
		t := re[3].Regexp().FindStringSubmatch(arg)
		if t == nil || strings.Contains(arg, "..") || t[1] != "" && !vars[t[1]] || t[1] == "" && !freshMktemp(arg) {
			return false
		}
		fresh = true
	}
	return fresh
}

// runsGit reports text holding any git invocation or nesting EachGit cannot see into, wherever it would run.
func runsGit(text string) bool {
	return shell.EachGit(text, "/", func(_, verb string, _ []string) bool { return verb != shell.WorktreeCommit })
}

// bannedGit is a hard rule 4 verb that writes the tree; `stash list|show`, `clean -n` and harmlessGit forms are fine.
func bannedGit(verb string, args []string) bool {
	readOnly := verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show") ||
		verb == "clean" && cleanDryRun(args) || harmlessGit(verb, args)
	return (bannedGitVerbs()[verb] || verb == shell.Uninspectable) && !readOnly
}

// cleanDryRun reports a git clean given -n/--dry-run, which removes nothing, and not -i, which prompts to delete.
func cleanDryRun(args []string) bool {
	dry := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return dry
		case len(a) > 2 && strings.HasPrefix("--dry-run", a): // git takes a unique prefix: --inter, --no-d but not --no
			dry = true
		case len(a) > 5 && strings.HasPrefix("--no-dry-run", a):
			dry = false
		case len(a) > 2 && strings.HasPrefix("--interactive", a):
			return false
		case len(a) > 2 && strings.HasPrefix("--exclude", a):
			i++ // the exclude pattern
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			flags, pattern, hasE := strings.Cut(a[1:], "e") // -e takes the rest of the cluster, else the next word
			if hasE && pattern == "" {
				i++
			}
			if strings.Contains(flags, "i") {
				return false
			}
			dry = dry || strings.Contains(flags, "n")
		}
	}
	return dry
}

// gitBannedUnder reports whether one git invocation is a banned verb targeting the harness tree.
func gitBannedUnder(target, verb string, args []string) bool {
	if !bannedGit(verb, args) {
		return false
	}
	worktrees := filepath.Join(paths.ConfigDir(), ".claude", "worktrees") // a linked worktree is its own checkout
	return paths.InsideDir(target, paths.ConfigDir()) &&
		(!paths.InsideDir(target, worktrees) || paths.SameFile(target, worktrees))
}
