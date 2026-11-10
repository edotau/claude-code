package safety

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// rewriteTimeout bounds the guard's git and process calls; safety-scan's budget is 5s.
const rewriteTimeout = 2 * time.Second

var rebaseResumeFlags = map[string]bool{"--continue": true, "--skip": true, "--abort": true, "--quit": true,
	"--edit-todo": true, "--show-current-patch": true}
var rebaseValueFlags = map[string]bool{"--onto": true, "-x": true, "--exec": true, "-s": true, "--strategy": true,
	"-X": true, "--strategy-option": true, "--empty": true, "--whitespace": true, "-C": true}

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
	return core.Block, core.SafetyMessage(in, core.Block, what, "", hint), "shared history rewrite", ""
}

// rewriteScan finds the first shared-tree rewrite in cmd, following `cd`, substitutions and nested `bash -c`/`eval`.
func rewriteScan(in *core.Input, cmd, cwd string) (rewriteRisk, bool) {
	var found rewriteRisk
	var warn string
	risky := shell.EachGit(dropRemoteHeredocs(cmd), cwd, func(target, verb string, args []string) bool {
		rng, ok := rewriteRange(verb, args)
		if !ok {
			return false
		}
		r, hit := assessRewrite(in, target, verb, rng)
		warn = cmp.Or(warn, r.warn)
		found = r
		return hit
	})
	if risky {
		return found, true
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
		return rebaseRange(args), true
	case "reset":
		if ops := rewriteOperands(args, nil); len(ops) > 0 && ops[0] != "" && !slices.Contains(args, "--") {
			return []string{ops[0] + "..HEAD"}, true
		}
	}
	return nil, false
}

// rebaseRange selects the explicit target branch, or HEAD when rebasing the current branch.
func rebaseRange(args []string) []string {
	operands := rewriteOperands(args, rebaseValueFlags)
	if slices.Contains(args, "--root") {
		if len(operands) > 0 {
			return []string{operands[0]}
		}
		return []string{"HEAD"}
	}
	base, branch := "@{upstream}", "HEAD"
	if len(operands) > 0 {
		base = operands[0]
	}
	if len(operands) > 1 {
		branch = operands[1]
	}
	return []string{base + ".." + branch}
}

// rewriteOperands collects non-option words before `--`, skipping option values.
func rewriteOperands(args []string, valueFlags map[string]bool) []string {
	var operands []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return operands
		case valueFlags[a]:
			i++
		case !strings.HasPrefix(a, "-"):
			operands = append(operands, a)
		}
	}
	return operands
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
	_, branch, nested := wthook.CheckoutOf(ctx, top)
	if verb == "rebase" {
		ref := rng[0]
		if _, tip, ok := strings.Cut(ref, ".."); ok {
			ref = tip
		}
		if ref != "HEAD" {
			branch = ref
		}
	}
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
	dirs, ok, err := core.Git(ctx, target, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir",
		"--git-common-dir")
	if err != nil || !ok || len(dirs) != 3 {
		return "", false, 0, err
	}
	top := core.CanonPath(dirs[0])
	n, err := rewriteCount(ctx, top, rng)
	return top, core.CanonPath(dirs[1]) != core.CanonPath(dirs[2]), n, err
}

// rewriteCount is how many commits rng holds in dir; 0 when its base is not a commit (a path, no upstream).
func rewriteCount(ctx context.Context, dir string, rng []string) (int, error) {
	if base, _, isRange := strings.Cut(rng[0], ".."); isRange {
		if _, ok, err := core.Git(ctx, dir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil || !ok {
			return 0, err
		}
	}
	out, ok, err := core.Git(ctx, dir, append([]string{"rev-list", "--count"}, rng...)...)
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.Atoi(out[0])
	return n, nil
}

// shareReasons lists why top is shared: the live config tree, peer sessions in it, or this session's leaves.
func shareReasons(ctx context.Context, in *core.Input, top string, linked bool, nested []string) ([]string, error) {
	var reasons []string
	if top == core.CanonPath(paths.ConfigDir()) {
		reasons = append(reasons, "it is the live config tree "+top)
	}
	procs, err := wthook.PeersOf(ctx, top, nested)
	for _, p := range procs {
		reasons = append(reasons, wthook.SessionLabel(p)+" works in it")
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

// runsWrittenFile matches a command that executes a file (a shell, source, ., exec, an interpreter or chmod +x).
var runsWrittenFile = regexp.MustCompile(`(?m)(^|[;&|\s])(bash|sh|zsh|source|exec|python3?|node|perl|ruby)\s|(^|[;&|])\s*\.\s|chmod\s+\+?[0-7]*x`)

// dropDataHeredocs removes quoted-delimiter `cat > f`/`tee f` heredoc bodies so hard rule 4 never asks on text written
// to a file; a command that also runs a file keeps every body (`cat <<'EOF' > f.sh … EOF && bash f.sh`).
func dropDataHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	if runsWrittenFile.MatchString(lines[0]) || heredocTail(lines) {
		return cmd
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		delim, ok := dataHeredocStart(lines[i])
		if !ok {
			continue
		}
		if end := heredocEnd(lines, i+1, delim, false); end > i {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}

// heredocTail reports any line after a heredoc's close (or any non-start line outside one) that runs a file.
func heredocTail(lines []string) bool {
	for i := 0; i < len(lines); i++ {
		delim, _, dash, ok := inertHeredocStart(lines[i])
		if !ok {
			delim, ok = dataHeredocStart(lines[i])
		}
		if ok {
			if end := heredocEnd(lines, i+1, delim, dash); end > i {
				i = end
				continue
			}
		}
		if i > 0 && runsWrittenFile.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// dataHeredocStart finds one quoted `<<'DELIM'` on a segment that is `cat > f` or `tee f`; other segments on the line
// (`mkdir -p d && cat > d/f <<'EOF'`) are checked as usual.
func dataHeredocStart(line string) (string, bool) {
	ms := heredocOp.FindAllStringSubmatch(line, -1)
	if len(ms) != 1 || ms[0][1] == "-" || ms[0][4] != "" {
		return "", false
	}
	for _, seg := range shell.Segments(line) {
		if !heredocOp.MatchString(seg) {
			continue
		}
		toks := shell.Tokens(heredocOp.ReplaceAllString(seg, " "))
		return ms[0][2] + ms[0][3], len(toks) > 0 && writesFile(toks)
	}
	return "", false
}
