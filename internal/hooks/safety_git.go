package hooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
)

// rewriteTimeout bounds the guard's git and process calls; safety-scan's budget is 5s.
const rewriteTimeout = 2 * time.Second

var rebaseResumeFlags = map[string]bool{"--continue": true, "--skip": true, "--abort": true, "--quit": true,
	"--edit-todo": true, "--show-current-patch": true}
var rebaseValueFlags = map[string]bool{"--onto": true, "-x": true, "--exec": true, "-s": true, "--strategy": true,
	"-X": true, "--strategy-option": true}

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

// rewriteRisk is one history rewrite: its verb, how many commits it rewrites, and why the tree is shared.
type rewriteRisk struct {
	verb, branch string
	commits      int
	reasons      []string
	warn         string
}

// rewriteVerdict blocks a history rewrite in a shared checkout; an infra error allows it and returns a warning.
func rewriteVerdict(in *core.Input) (d core.Decision, msg, detail, warn string) {
	r, risky := rewriteScan(in, in.ToolCommand(), in.CWD)
	if !risky {
		return core.Allow, "", "", r.warn
	}
	what := fmt.Sprintf("git %s rewrites %d commit(s) on %s in a shared checkout: %s", r.verb, r.commits, r.branch,
		strings.Join(r.reasons, "; "))
	hint := "Make a new commit instead; undo a commit with git revert. " +
		"To rewrite history, do it in your own linked worktree: claude-code worktree create <slug>."
	return core.Block, safetyMessage(in, core.Block, what, "", hint), "shared history rewrite", ""
}

// rewriteScan finds the first shared-tree rewrite in cmd, following `cd`, substitutions and nested `bash -c`/`eval`.
func rewriteScan(in *core.Input, cmd, cwd string) (rewriteRisk, bool) {
	masked, subs := extractSubstitutions(dropRemoteHeredocs(cmd))
	r, ok := firstRewrite(in, subs, cwd) // a substitution's cd only affects its own nested evaluation
	if ok {
		return r, true
	}
	warn := r.warn
	for _, seg := range shellSegments(masked) {
		toks := shellTokens(seg)
		if len(toks) == 0 || toks[0] == "ssh" { // ssh runs its command on the remote host
			continue
		}
		if next, ok := cdTarget(toks, cwd); ok {
			cwd = next
			continue
		}
		r, ok := segmentRewrite(in, toks, cwd)
		if ok {
			return r, true
		}
		warn = cmp.Or(warn, r.warn)
	}
	return rewriteRisk{warn: warn}, false
}

// firstRewrite scans each command from cwd and returns the first risky rewrite, else the first warning.
func firstRewrite(in *core.Input, cmds []string, cwd string) (rewriteRisk, bool) {
	var warn string
	for _, c := range cmds {
		r, ok := rewriteScan(in, c, cwd)
		if ok {
			return r, true
		}
		warn = cmp.Or(warn, r.warn)
	}
	return rewriteRisk{warn: warn}, false
}

// segmentRewrite checks one segment's nested shell command and each of its git invocations.
func segmentRewrite(in *core.Input, toks []string, cwd string) (rewriteRisk, bool) {
	var warn string
	if nested, ok := nestedShellCommand(toks); ok {
		r, risky := rewriteScan(in, nested, cwd)
		if risky {
			return r, true
		}
		warn = r.warn
	}
	for i, t := range toks {
		if t != "git" {
			continue
		}
		target, verb, args := gitInvocation(toks[i+1:], cwd)
		rng, ok := rewriteRange(verb, args)
		if !ok {
			continue
		}
		r, risky := assessRewrite(in, target, verb, rng)
		if risky {
			return r, true
		}
		warn = cmp.Or(warn, r.warn)
	}
	return rewriteRisk{warn: warn}, false
}

// rewriteRange is the rev-list range a git invocation would rewrite; ok=false means it rewrites nothing.
func rewriteRange(verb string, args []string) ([]string, bool) {
	switch verb {
	case "commit":
		if slices.Contains(args, "--amend") {
			return []string{"--max-count=1", "HEAD"}, true
		}
	case "rebase":
		if slices.ContainsFunc(args, func(a string) bool { return rebaseResumeFlags[a] }) {
			return nil, false // acts on a rebase whose start was already checked
		}
		if slices.Contains(args, "--root") {
			return []string{"HEAD"}, true
		}
		return []string{cmp.Or(firstOperand(args, rebaseValueFlags), "@{upstream}") + "..HEAD"}, true
	case "reset":
		if base := firstOperand(args, nil); base != "" && !slices.Contains(args, "--") {
			return []string{base + "..HEAD"}, true
		}
	}
	return nil, false
}

// firstOperand is the first non-option word before `--`, skipping the value of each flag in valueFlags.
func firstOperand(args []string, valueFlags map[string]bool) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return ""
		case valueFlags[a]:
			i++
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
}

// assessRewrite is risky when the target tree is shared and the range holds a commit; errors fail open with a warning.
func assessRewrite(in *core.Input, target, verb string, rng []string) (rewriteRisk, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), rewriteTimeout)
	defer cancel()
	top, linked, n, err := probeRewrite(ctx, target, rng)
	if err != nil {
		return rewriteRisk{warn: rewriteWarn(verb, err)}, false // fail open
	}
	if n == 0 {
		return rewriteRisk{}, false // not a repo, a non-commit operand or an empty range: git rewrites nothing
	}
	_, branch, nested := checkoutOf(ctx, top)
	reasons, err := shareReasons(ctx, in, top, linked, nested)
	r := rewriteRisk{verb: verb, branch: branch, commits: n, reasons: reasons}
	if err != nil {
		r.warn = rewriteWarn(verb, err)
	}
	return r, len(reasons) > 0
}

// rewriteWarn is the stderr line for a rewrite the guard could not check.
func rewriteWarn(verb string, err error) string {
	return fmt.Sprintf("safety: WARNING — could not check git %s for a shared-history rewrite (%v); allowed\n", verb, err)
}

// probeRewrite resolves target's toplevel, whether it is a linked worktree, and the rewrite's commit count.
func probeRewrite(ctx context.Context, target string, rng []string) (string, bool, int, error) {
	dirs, ok, err := gitOut(ctx, target, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir",
		"--git-common-dir")
	if err != nil || !ok || len(dirs) != 3 {
		return "", false, 0, err
	}
	top := canonPath(dirs[0])
	n, err := rewriteCount(ctx, top, rng)
	return top, isLinkedWorktree(dirs[1], dirs[2]), n, err
}

// rewriteCount is how many commits rng holds in dir; 0 when its base is not a commit (a path, no upstream).
func rewriteCount(ctx context.Context, dir string, rng []string) (int, error) {
	if base, isRange := strings.CutSuffix(rng[0], "..HEAD"); isRange {
		if ok, err := isCommit(ctx, dir, base); err != nil || !ok {
			return 0, err
		}
	}
	out, ok, err := gitOut(ctx, dir, append([]string{"rev-list", "--count"}, rng...)...)
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.Atoi(out[0])
	return n, nil
}

// isCommit reports whether rev names a commit in dir; false for a path or a missing upstream.
func isCommit(ctx context.Context, dir, rev string) (bool, error) {
	_, ok, err := gitOut(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return ok, err
}

// isLinkedWorktree is true when the git dir is not the common dir: a `git worktree add` checkout.
func isLinkedWorktree(gitDir, commonDir string) bool {
	return canonPath(gitDir) != canonPath(commonDir)
}

// shareReasons lists why top is shared: the live config tree, peer sessions in it, or this session's leaves.
func shareReasons(ctx context.Context, in *core.Input, top string, linked bool, nested []string) ([]string, error) {
	var reasons []string
	if top == canonPath(paths.ConfigDir()) {
		reasons = append(reasons, "it is the live config tree "+top)
	}
	procs, err := peersOf(ctx, top, nested)
	for _, p := range procs {
		reasons = append(reasons, sessionLabel(p)+" works in it")
	}
	if linked {
		return reasons, err
	}
	if in.AgentID != "" {
		return append(reasons, "you are a leaf in the main tree"), err
	}
	if owner := inflight.SessionOwner(); owner != 0 {
		if n := inflight.CountOwned(owner, time.Now()); n > 0 {
			reasons = append(reasons, fmt.Sprintf("this session has %d live leaf agent(s) in the main tree", n))
		}
	}
	return reasons, err
}

// nestedIn keeps the worktree paths inside top: the peer scan skips those, never the tree that holds top.
func nestedIn(top string, worktrees []string) []string {
	var out []string
	for _, w := range worktrees {
		if strings.HasPrefix(w, top+string(filepath.Separator)) {
			out = append(out, w)
		}
	}
	return out
}

// gitOut runs git in dir: ok=false when git itself refused; err for a missing binary or a timeout.
func gitOut(ctx context.Context, dir string, args ...string) ([]string, bool, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case errors.As(err, &exit):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), true, nil
}

// gitInvocation parses git's global options up to the verb: the target tree, the verb, the verb's words.
func gitInvocation(toks []string, cwd string) (target, verb string, args []string) {
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
	if p, ok := resolveShellPath(workTree, cwd); workTree != "" && ok {
		return p
	}
	if p, ok := resolveShellPath(gitDir, cwd); gitDir != "" && ok && filepath.Base(p) == ".git" {
		return filepath.Dir(p)
	}
	target := cwd
	for _, c := range cPaths { // successive -C paths apply relative to each other
		if p, ok := resolveShellPath(c, target); ok {
			target = p
		}
	}
	return target
}
