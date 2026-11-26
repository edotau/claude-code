package docs

import (
	"bytes"
	"cmp"
	"context"
	"errors"

	"github.com/edotau/claude-code/internal/memory"

	"fmt"
	"io"
	"net/url"
	"os"
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
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/worktree"
)

var gitDirArg = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bgit\s+(?:-c\s+(?:"[^"]*"|'[^']*'|\S+)\s+|-\S+\s+)*?-C\s+("[^"]+"|'[^']+'|\S+)`)
})

var cmdSegment = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`&&|\|\||[;|\n]`) })

var leadingCD = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|[^\s&;]+)\s*&&`)
})

const harnessCommitPattern = `(?:^|[^\w.-])(?:claude-code|\.hookd)\s+worktree\s+commit\b`

// tickMatch matches git or harness commits, alone or behind rtk or a && chain.
var tickMatch = core.Gate(`\bgit\b[^;&|\n]*\bcommit\b|`+harnessCommitPattern, "commit")

// planDone matches a `[rtk] claude-code plan task ... --done` command, whose tick is written before the hook runs.
var planDone = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)(?:^|[;&|(])\s*(?:rtk\s+)?(?:\S*/)?(?:claude-code|\.hookd)\s+plan\s+task\b[^;&|\n]*\s--done\b`)
})

// freeSlots and readyClaims are seams: the in-flight cap less the live agents, and every other party's claims.
var (
	freeSlots   = func() int { return max(router.CurrentLimits().Total-inflight.Count().Total, 0) }
	readyClaims = func(in *core.Input) map[string][]string {
		hits, _ := worktree.LiveClaims(worktree.ClaimSource{Alive: proc.Alive, Self: worktree.SessionSlug(in.SessionID),
			Owner: inflight.SessionOwner, Leaf: func(c worktree.Claim) string {
				if c.Agent == "" || time.Since(time.Unix(c.TS, 0)) > inflight.Idle {
					return ""
				}
				return "leaf " + c.Agent[:min(8, len(c.Agent))]
			}, Label: func(pid int) string {
				return worktree.SessionLabel(worktree.Proc{PID: pid, Comm: cmp.Or(proc.Comm(pid), "claude")})
			}})
		out := make(map[string][]string, len(hits))
		for p, hs := range hits {
			for _, h := range hs {
				out[p] = append(out[p], h.Who)
			}
		}
		return out
	}
)

// Tick advances named tasks after commits land on main, then lists the ready ones; advisory, parse edges accepted.
func Tick(in *core.Input) core.Result {
	res := tickEvent(in)
	cmd := in.ToolCommand()
	if !strings.Contains(res.Context, "plans: ticked") && (in.ToolFailed() || !planDone().MatchString(cmd)) {
		return res
	}
	main := ticksRepo(in.CWD, cmd)
	if main == "" {
		return res
	}
	n := plans.ReadyNotice(plans.ReadyInput{Root: main, Slots: freeSlots(), Claims: func() map[string][]string { return readyClaims(in) }})
	res.Context = core.JoinContext(res.Context, n)
	return res
}

var anyDirArg = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\s-C\s+("[^"]+"|'[^']+'|\S+)`) })

var cdCall = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|[^\w./-])(?:cd|pushd)\s`) })

// relativeRepeatedC reports several -C args with a relative one, which git resolves cumulatively.
func relativeRepeatedC(cmd string) bool {
	ms := anyDirArg().FindAllStringSubmatch(cmd, -1)
	return len(ms) > 1 && slices.ContainsFunc(ms, func(m []string) bool {
		return !filepath.IsAbs(paths.ExpandHome(strings.Trim(m[1], `"'`)))
	})
}

// ticksRepo is the one main checkout cmd's repos resolve to; "" when none or several, so no other repo is named.
func ticksRepo(cwd, cmd string) string {
	cands, base := commitCandidates(cwd, cmd), cwd
	if m := leadingCD().FindStringSubmatch(cmd); m != nil {
		cands, base = cands[:len(cands)-1], candidateDir(strings.Trim(m[1], `"'`), cwd) // the cd target replaces cwd
	}
	for _, m := range anyDirArg().FindAllStringSubmatch(cmd, -1) { // every -C names a repo, whatever the verb
		cands = append(cands, candidateDir(strings.Trim(m[1], `"'`), base))
	}
	if len(cdCall().FindAllString(cmd, 2)) > len(leadingCD().FindStringSubmatch(cmd))/2 || relativeRepeatedC(cmd) { // a cd past the leading one or a chained relative -C: unknowable
		return ""
	}
	var main string
	for _, d := range cands {
		_, m := paths.CheckoutOf(d)
		if m == "" || m == main {
			continue
		}
		if main != "" {
			return ""
		}
		main = m
	}
	return main
}

func tickEvent(in *core.Input) core.Result {
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
	shas := landedShas(in, cmd, out)
	for _, sha := range shas {
		blocks = append(blocks, tickCommit(in, cmd, sha, &stderr)...)
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	res.Context = core.JoinContext(blocks...)
	return res
}

// landedShas is the commit shas cmd's output names, else the recent HEAD for a commit whose output has none.
func landedShas(in *core.Input, cmd, out string) []string {
	var shas []string
	for _, re := range []*regexp.Regexp{core.CommitLine(), rtkCommitLine()} {
		for _, m := range re.FindAllStringSubmatch(out, -1) {
			if !slices.Contains(shas, m[1]) {
				shas = append(shas, m[1])
			}
		}
	}
	if len(shas) > 0 || in.ToolFailed() || !landsQuietly(cmd, out) {
		return shas
	}
	if sha := recentHead(in.CWD, cmd); sha != "" {
		shas = append(shas, sha)
	}
	return shas
}

// landsQuietly reports a real commit command whose output cannot name its sha.
func landsQuietly(cmd, out string) bool {
	return verbCommit().MatchString(cmd) || realCommit(cmd) && (strings.TrimSpace(out) == "" || quietFlag().MatchString(cmd))
}

// rtkCommitLine matches the `ok <sha>` line rtk prints in place of git's `[branch sha] subject` line.
var rtkCommitLine = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^ok ([0-9a-f]{7,40})\s*$`) })

// verbCommit matches the worktree commit verb, whose output carries no sha line.
var verbCommit = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(harnessCommitPattern) })

// quietFlag matches commit's -q/--quiet (alone or bundled), which prints no summary line to find a sha in.
var quietFlag = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\s(?:--quiet|-[a-zA-Z]*q[a-zA-Z]*)(?:\s|$)`) })

// commitRun matches a real `git [-C d|-c k=v|--flag]... commit`, not a log or grep that mentions the word.
var commitRun = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[\s;&|(])git(?:\s+(?:-[Cc]\s+(?:"[^"]*"|'[^']*'|\S+)|--?\S+))*\s+commit\b`)
})

// realCommit reports whether cmd runs a git commit that is not a dry run.
func realCommit(cmd string) bool {
	return shell.EachGit(cmd, "", func(_ string, verb string, args []string) bool {
		return verb == "commit" && !strings.Contains(strings.Join(args, " "), "--dry-run")
	})
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
	top := toplevel(dir)
	benchOnCommit(ctx, in, dir, sha)
	results, tied, err := plans.DeferTick(top, sha, nil, in.SessionID)
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
	return paths.AnchorPath(paths.ExpandHome(p), base)
}

var briefTaskRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?mi)^[\s>*-]*Task:[ \t]*(\S.*?)\s*$`)
})

var nonWordRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[^\p{L}\p{N}.]+`) })

// PlanLookup is planCands' result for a spawn, read ahead on its own goroutine; nil means nothing to claim.
type PlanLookup struct {
	done  chan struct{}
	cands []planCand
}

// PrefetchPlan starts the read-only plan lookup ClaimPlanTask needs, so it overlaps the caller's other reads.
func PrefetchPlan(in *core.Input) *PlanLookup {
	if !core.SpawnTools[in.ToolName] || in.Nested("tool_use_id") == "" {
		return nil
	}
	l := &PlanLookup{done: make(chan struct{})}
	go func() {
		defer close(l.done)
		l.cands = planCands(in.CWD, briefPlan(in.Nested("tool_input", "prompt")))
	}()
	return l
}

// wait is the prefetched lookup, or a direct one when nothing was prefetched.
func (l *PlanLookup) wait(cwd, brief string) []planCand {
	if l == nil {
		return planCands(cwd, briefPlan(brief))
	}
	<-l.done
	return l.cands
}

// ClaimPlanTask claims the first candidate plan's task a spawn names (`Task:` line or unique title match), else nothing.
func ClaimPlanTask(in *core.Input, id string, pre *PlanLookup) error {
	if !core.SpawnTools[in.ToolName] || id == "" {
		return nil
	}
	brief := in.Nested("tool_input", "prompt")
	for _, c := range pre.wait(in.CWD, brief) {
		task, ok := matchBriefTask(c.parsed, brief, in.Nested("tool_input", "description"))
		if !ok {
			continue
		}
		if task.Claim != "" { // the locked ClaimFree below re-checks
			return nil
		}
		agent := cmp.Or(in.Nested("tool_input", "subagent_type"), in.ToolName)
		text := fmt.Sprintf("working: %s %s · worktree %s · since %s", agent, id, cmp.Or(c.worktreeName, "main"), time.Now().Format("15:04"))
		if err := plans.ClaimFree(c.plan, taskKey(task), text); err != nil {
			return fmt.Errorf("plan-claim: %w", err)
		}
		if err := recordClaim(id, c.plan); err != nil {
			return fmt.Errorf("plan-claim: record: %w", err)
		}
		if c.worktreeName == "" {
			return nil
		}
		if _, err := worktree.SetLedgerTask(in.CWD, taskKey(task)+" in "+strings.TrimSuffix(filepath.Base(c.plan), ".md"), time.Now()); err != nil {
			return fmt.Errorf("plan-claim: ledger: %w", err)
		}
		return nil
	}
	return nil
}

// ClearPlanClaim removes the claim a stopping agent's spawn wrote, found by its tool_use_id in the claim text.
func ClearPlanClaim(cwd, id string) error {
	if id == "" {
		return nil
	}
	rec := claimRecord(id)
	claimed, _ := os.ReadFile(rec) // the plan ClaimPlanTask claimed in; none falls back to the default plans
	var errs []error
	for _, c := range planCands(cwd, string(claimed)) {
		for _, t := range c.parsed.Tasks {
			if strings.Contains(t.Claim, " "+id+" ") {
				errs = append(errs, plans.ClearClaimOwned(c.plan, taskKey(t), " "+id+" "))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("plan-claim: release: %w", err)
	}
	if err := os.Remove(rec); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("plan-claim: release: %w", err)
	}
	return nil
}

// claimRecord is where ClaimPlanTask notes the plan a spawn's claim went to, for the clear that has no brief.
func claimRecord(id string) string {
	return filepath.Join(paths.StateDir(), "plan-claims", url.PathEscape(id))
}

func recordClaim(id, plan string) error {
	rec := claimRecord(id)
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		return err
	}
	return os.WriteFile(rec, []byte(plan), 0o600)
}

// toplevel is dir's checkout root: a stat walk first, git only for layouts it cannot see.
func toplevel(dir string) string {
	top, _ := paths.CheckoutOf(dir)
	if top == "" {
		return memory.GitToplevel(dir) // a bare gitdir layout the stat walk cannot see
	}
	return paths.Real(top) // git prints the symlink-resolved toplevel
}

// planCand is a plan a spawn's task may live in; worktreeName is cwd's lane when it is that lane's repo plan.
type planCand struct {
	plan         string
	parsed       plans.Plan
	worktreeName string
}

// planCands is the named plan alone, else cwd's repo active plan then the harness's, each once; it never writes.
func planCands(cwd, named string) []planCand {
	top, wt := toplevel(cwd), ""
	if top != "" {
		wt = plans.WorktreeName(top)
	}
	var out []planCand
	add := func(p string, parsed plans.Plan) {
		if p == "" || slices.ContainsFunc(out, func(o planCand) bool { return paths.SameFile(o.plan, p) }) {
			return
		}
		c := planCand{plan: p, parsed: parsed}
		if top != "" && paths.SameFile(filepath.Dir(p), plans.PeekHome(top)) {
			c.worktreeName = wt
		}
		out = append(out, c)
	}
	if named != "" {
		parsed, _ := plans.Parse(named) // an unreadable named plan has no tasks to claim, and no fallback
		add(named, parsed)
		return out
	}
	if top != "" {
		if p, parsed, ok := plans.Active(plans.PeekHome(top), wt, plans.MirrorDests()); ok {
			add(p, parsed)
		}
	}
	if p, parsed, ok := plans.Active(plans.PeekHome(paths.ConfigDir()), "", plans.MirrorDests()); ok {
		add(p, parsed)
	}
	return out
}

// briefPlan is the absolute plan file a brief names, else "".
func briefPlan(brief string) string {
	m := briefPlanRe().FindStringSubmatch(brief)
	if m == nil || !filepath.IsAbs(paths.ExpandHome(m[1])) {
		return ""
	}
	return paths.ExpandHome(m[1])
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
	top := toplevel(dir)
	fresh, err := plans.ClaimLanding(top, before, after)
	if err != nil {
		return core.Result{Stderr: fmt.Sprintf("plan-tick: %v\n", err)}
	}
	if !fresh {
		return core.Result{}
	}
	BenchLanded(ctx, top, before, after, in.SessionID) // a fast-forward fires no commit hook
	var notes bytes.Buffer
	err = plans.TickRange(ctx, top, "", before+".."+after, "", plans.MirrorDests(), false, &notes)
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

// BriefTask is the first candidate plan's open task a hand-back worked: the dispatch's plan_task, else its Task: line.
func BriefTask(cwd, brief, planTask string) (plans.Task, bool) {
	for _, c := range planCands(cwd, briefPlan(brief)) {
		if t, ok := PlanBriefTask(c.parsed, brief, planTask); ok {
			return t, true
		}
	}
	return plans.Task{}, false
}

// PlanBriefTask is BriefTask over an already parsed active plan.
func PlanBriefTask(parsed plans.Plan, brief, planTask string) (plans.Task, bool) {
	if planTask != "" {
		return matchTaskByID(parsed, normTask(planTask))
	}
	if m := briefTaskRe().FindStringSubmatch(brief); m != nil {
		return matchTaskByID(parsed, normTask(m[1]))
	}
	return plans.Task{}, false
}

var (
	briefPlanRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(~?/\S*plans/\S+?\.md)`) })
	taskOfRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bTask\s+([\w.]+)\s+of\b`) })
)

// NamedPlanTask is the open task of the plan a brief names, never another plan's; named is false if none.
func NamedPlanTask(brief, planTask string) (planPath string, task plans.Task, named, ok bool) {
	m := briefPlanRe().FindStringSubmatch(brief)
	if m == nil {
		return "", plans.Task{}, false, false
	}
	planPath = paths.ExpandHome(m[1])
	parsed, err := plans.Parse(planPath)
	if err != nil {
		return planPath, plans.Task{}, true, false
	}
	id := planTask
	if t := briefTaskRe().FindStringSubmatch(brief); t != nil {
		id = t[1]
	} else if t := taskOfRe().FindStringSubmatch(brief); t != nil {
		id = t[1]
	}
	task, ok = matchTaskByID(parsed, normTask(id))
	return planPath, task, true, ok
}
