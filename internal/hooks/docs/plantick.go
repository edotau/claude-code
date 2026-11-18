package docs

import (
	"bytes"
	"cmp"
	"context"
	"errors"

	"github.com/edotau/claude-code/internal/memory"

	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

var gitDirArg = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bgit\s+(?:-c\s+(?:"[^"]*"|'[^']*'|\S+)\s+|-\S+\s+)*?-C\s+("[^"]+"|'[^']+'|\S+)`)
})

var cmdSegment = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`&&|\|\||[;|\n]`) })

var leadingCD = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|[^\s&;]+)\s*&&`)
})

// tickMatch matches a `git commit` or the `claude-code worktree commit` verb, alone or behind rtk or a && chain.
var tickMatch = core.Gate(`\bgit\b[^;&|\n]*\bcommit\b|\bclaude-code\s+worktree\s+commit\b`, "commit")

// Tick advances named tasks only after their commits land on main.
func Tick(in *core.Input) core.Result {
	cmd := in.ToolCommand()
	if in.ToolName != "Bash" {
		return core.Result{}
	}
	if !tickMatch.MatchString(cmd) {
		return tickFastForward(in, cmd)
	}
	out := in.Nested("tool_response", "stdout")
	if out == "" {
		out = in.ToolResponseText()
	}
	ff := tickFastForward(in, cmd)
	var blocks []string
	if ff.Context != "" {
		blocks = append(blocks, ff.Context)
	}
	var stderr bytes.Buffer
	stderr.WriteString(ff.Stderr)
	var shas []string
	for _, m := range core.CommitLine().FindAllStringSubmatch(out, -1) {
		if !slices.Contains(shas, m[1]) {
			shas = append(shas, m[1])
		}
	}
	if len(shas) == 0 && !in.ToolFailed() && realCommit(cmd) && (strings.TrimSpace(out) == "" || quietFlag().MatchString(cmd)) {
		if sha := recentHead(in.CWD, cmd); sha != "" {
			shas = append(shas, sha)
		}
	}
	for _, sha := range shas {
		blocks = append(blocks, tickCommit(in, cmd, sha, &stderr)...)
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	res.Context = core.JoinContext(blocks...)
	return res
}

// quietFlag matches commit's -q/--quiet (alone or bundled), which prints no summary line to find a sha in.
var quietFlag = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\s(?:--quiet|-[a-zA-Z]*q[a-zA-Z]*)(?:\s|$)`) })

// commitRun matches a real `git [-C d|-c k=v|--flag]... commit`, not a log or grep that mentions the word.
var commitRun = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[\s;&|(])git(?:\s+(?:-[Cc]\s+(?:"[^"]*"|'[^']*'|\S+)|--?\S+))*\s+commit\b`)
})

// realCommit reports whether cmd runs a git commit that is not a dry run.
func realCommit(cmd string) bool {
	for _, seg := range cmdSegment().Split(cmd, -1) {
		if commitRun().MatchString(seg) && !strings.Contains(seg, "--dry-run") {
			return true
		}
	}
	return false
}

// recentWindow is how young HEAD must be for a quiet commit's fallback; the payload gives no call start time.
const recentWindow = 2 * time.Minute

// recentHead is the HEAD sha of the first candidate repo whose HEAD was committed just now, else "".
func recentHead(cwd, cmd string) string {
	ctx, cancel := core.HookContext("docs-plan-tick")
	defer cancel()
	for _, d := range commitCandidates(cwd, cmd) {
		if d == "" {
			continue
		}
		out, err := proc.RunGit(ctx, d, "log", "-1", "--format=%H %ct", "HEAD")
		if err != nil {
			continue
		}
		sha, ct, ok := strings.Cut(strings.TrimSpace(out), " ")
		if sec, perr := strconv.ParseInt(ct, 10, 64); ok && perr == nil && time.Since(time.Unix(sec, 0)) < recentWindow {
			return sha
		}
	}
	return ""
}

// tickCommit ticks every plan one landed commit advances and returns the context blocks.
func tickCommit(in *core.Input, cmd, sha string, stderr io.Writer) []string {
	ctx, cancel := core.HookContext("docs-plan-tick")
	defer cancel()
	dir := commitRepoDir(ctx, in.CWD, cmd, sha)
	if dir == "" {
		return nil
	}
	top := memory.GitToplevel(dir)
	results, tied, err := plans.TickCommit(top, sha, nil)
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
	}
	var blocks []string
	for _, r := range results {
		blocks = append(blocks, r.Lines(sha)...)
	}
	if n := plans.TieNote(tied); n != "" {
		blocks = append(blocks, n)
	}
	return blocks
}

// commitRepoDir is the candidate dir (commit -C args, leading cd, cwd) whose HEAD is sha, else the first holding it.
func commitRepoDir(ctx context.Context, cwd, cmd, sha string) string {
	var holders []string
	for _, d := range commitCandidates(cwd, cmd) {
		if d == "" {
			continue
		}
		out, err := proc.RunGit(ctx, d, "rev-parse", "HEAD", sha+"^{commit}") // fails unless d's repo holds the commit
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(out), sha) {
			return d // linked worktrees share objects; the one whose HEAD is the commit made it
		}
		holders = append(holders, d)
	}
	if len(holders) > 0 {
		return holders[0]
	}
	return ""
}

// commitCandidates lists dirs a commit may have run in: -C args, a leading cd, then cwd; a relative -C uses the cd.
func commitCandidates(cwd, cmd string) []string {
	base, cdFound := cwd, false
	if m := leadingCD().FindStringSubmatch(cmd); m != nil {
		base, cdFound = candidateDir(strings.Trim(m[1], `"'`), cwd), true
	}
	var cands []string
	for _, seg := range cmdSegment().Split(cmd, -1) { // only commit invocations name a candidate repo
		if !core.GitCommitCmd().MatchString(seg) && !strings.Contains(seg, "merge") && !strings.Contains(seg, "pull") {
			continue
		}
		if m := gitDirArg().FindStringSubmatch(seg); m != nil {
			cands = append(cands, candidateDir(strings.Trim(m[1], `"'`), base))
		}
	}
	if cdFound {
		cands = append(cands, base)
	}
	return append(cands, cwd)
}

// candidateDir expands a leading ~ or ~/ and anchors a relative path to base.
func candidateDir(p, base string) string {
	return core.AnchorPath(paths.ExpandHome(p), base)
}

var briefTaskRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?mi)^[\s>*-]*Task:[ \t]*(\S.*?)\s*$`)
})

var nonWordRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[^\p{L}\p{N}.]+`) })

// ClaimPlanTask claims the active plan's task a spawn names (`Task:` line or unique title match), else nothing.
func ClaimPlanTask(in *core.Input, id string) error {
	if !core.SpawnTools[in.ToolName] || id == "" {
		return nil
	}
	plan, parsed, worktreeName, ok := activePlan(in.CWD)
	if !ok {
		return nil
	}
	task, ok := matchBriefTask(parsed, in.Nested("tool_input", "prompt"), in.Nested("tool_input", "description"))
	if !ok || task.Claim != "" { // the locked ClaimFree below re-checks
		return nil
	}
	agent := cmp.Or(in.Nested("tool_input", "subagent_type"), in.ToolName)
	text := fmt.Sprintf("working: %s %s · worktree %s · since %s", agent, id, cmp.Or(worktreeName, "main"), time.Now().Format("15:04"))
	if err := plans.ClaimFree(plan, taskKey(task), text); err != nil {
		return fmt.Errorf("plan-claim: %w", err)
	}
	if worktreeName == "" {
		return nil
	}
	if _, err := worktree.SetLedgerTask(in.CWD, taskKey(task)+" in "+strings.TrimSuffix(filepath.Base(plan), ".md"), time.Now()); err != nil {
		return fmt.Errorf("plan-claim: ledger: %w", err)
	}
	return nil
}

// ClearPlanClaim removes the claim a stopping agent's spawn wrote, found by its tool_use_id in the claim text.
func ClearPlanClaim(cwd, id string) error {
	if id == "" {
		return nil
	}
	plan, parsed, _, ok := activePlan(cwd)
	if !ok {
		return nil
	}
	var errs []error
	for _, t := range parsed.Tasks {
		if strings.Contains(t.Claim, " "+id+" ") {
			errs = append(errs, plans.ClearClaimOwned(plan, taskKey(t), " "+id+" "))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("plan-claim: release: %w", err)
	}
	return nil
}

// activePlan is cwd's repo plan with an open owned task, parsed, plus its linked worktree's name ("" in main).
func activePlan(cwd string) (plan string, parsed plans.Plan, worktreeName string, ok bool) {
	top := memory.GitToplevel(cwd)
	if top == "" {
		return "", plans.Plan{}, "", false
	}
	worktreeName = plans.WorktreeName(top)
	plan, parsed, ok = plans.Active(plans.Home(top), worktreeName, plans.MirrorDests())
	return plan, parsed, worktreeName, ok
}

func taskKey(t plans.Task) string { return cmp.Or(t.ID, t.Title) }

func normTask(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "phase ")
	return strings.Trim(nonWordRe().ReplaceAllString(s, " "), " .")
}

// matchBriefTask returns the one open task a `Task:` line names (id or exact title), else the description's title match
func matchBriefTask(p plans.Plan, brief, description string) (plans.Task, bool) {
	if m := briefTaskRe().FindStringSubmatch(brief); m != nil {
		return matchTaskByID(p, normTask(m[1]))
	}
	return matchTaskByTitle(p, normTask(description))
}

func matchTaskByID(p plans.Plan, want string) (plans.Task, bool) {
	var hit []plans.Task
	for _, t := range p.Tasks {
		if !t.Done && !t.Dropped && want != "" && (normTask(t.ID) == want || normTask(t.Title) == want) {
			hit = append(hit, t)
		}
	}
	return only(hit)
}

func matchTaskByTitle(p plans.Plan, want string) (plans.Task, bool) {
	if len(strings.Fields(want)) < 3 {
		return plans.Task{}, false
	}
	var hit []plans.Task
	for _, t := range p.Tasks {
		if title := normTask(t.Title); !t.Done && !t.Dropped && (title == want || strings.Contains(title, want)) {
			hit = append(hit, t)
		}
	}
	return only(hit)
}

func only(ts []plans.Task) (plans.Task, bool) {
	if len(ts) != 1 {
		return plans.Task{}, false
	}
	return ts[0], true
}

// ClearStoppedClaim resolves the stopping agent's spawn id from its meta.json and clears that spawn's plan claim.
func ClearStoppedClaim(in *core.Input, dirs []string, id string) error {
	for _, d := range dirs {
		if tu := inflight.ToolUseID(d, id); tu != "" {
			return ClearPlanClaim(in.CWD, tu)
		}
	}
	return nil
}

func tickFastForward(in *core.Input, cmd string) core.Result {
	if !strings.Contains(cmd, "merge") && !strings.Contains(cmd, "pull") {
		return core.Result{}
	}
	ctx, cancel := core.HookContext("docs-plan-tick")
	defer cancel()
	before, after, dir := landedRange(ctx, in.CWD, cmd)
	if dir == "" {
		return core.Result{}
	}
	fresh, err := plans.ClaimLanding(memory.GitToplevel(dir), before, after)
	if err != nil {
		return core.Result{Stderr: fmt.Sprintf("plan-tick: %v\n", err)}
	}
	if !fresh {
		return core.Result{}
	}
	var notes bytes.Buffer
	err = plans.TickRange(ctx, memory.GitToplevel(dir), "", before+".."+after, "", plans.MirrorDests(), false, &notes)
	res := core.Result{Context: notes.String()}
	if err != nil && !errors.Is(err, plans.ErrNotOnTrunk) { // a lane or a feature branch ticks nothing, by design
		res.Stderr = fmt.Sprintf("plan-tick: %v\n", err)
	}
	return res
}

var landingRun = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(strings.Replace(commitRun().String(), `\s+commit\b`, `\s+(?:merge|pull)\b`, 1))
})

// landingEntry matches a HEAD reflog subject (and its unix time) that a merge or pull wrote when it moved HEAD.
var landingEntry = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(?:merge|pull)[^:\x00]*: (?:Fast-forward|Merge made by)[^\x00]*\x00HEAD@\{(\d+)\}$`)
})

// landedRange is the HEAD move a fresh merge or pull left in the reflog, whatever it printed or a later command did.
func landedRange(ctx context.Context, cwd, cmd string) (before, after, dir string) {
	if !landingRun().MatchString(cmd) {
		return "", "", ""
	}
	for _, d := range commitCandidates(cwd, cmd) {
		raw, err := proc.RunGit(ctx, d, "reflog", "-2", "--date=unix", "--format=%H%x00%gs%x00%gd", "HEAD")
		if d == "" || err != nil {
			continue
		}
		rows := strings.Split(strings.TrimSpace(raw), "\n")
		if len(rows) != 2 {
			continue
		}
		after, rest, _ := strings.Cut(rows[0], "\x00")
		before, _, _ := strings.Cut(rows[1], "\x00")
		if m := landingEntry().FindStringSubmatch(rest); m != nil {
			if sec, perr := strconv.ParseInt(m[1], 10, 64); perr == nil && time.Since(time.Unix(sec, 0)) < recentWindow {
				return before, after, d
			}
		}
	}
	return "", "", ""
}
