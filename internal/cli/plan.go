package cli

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/review/planlint"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

var planPage = helptext.Page{
	Usage: "claude-code plan <command> [flags]",
	About: "Plan checklists (ticked/total, live claims) and plan detectors for vague steps and goals.",
	Commands: [][2]string{
		{"status", "progress per plan; --all spans every repo under the plans home"},
		{"tick", "tick tasks in --range <a>..<b>; --audit closes and archives plans"},
		{"task", "tick, reopen or strike one task by hand"},
		{"ready", "open tasks a leaf can take now, with the free dispatch slots"},
		{"assumptions", "vague or hopeful steps in a plan"},
		{"goals", "plan steps with no verifiable goal"},
	},
	Hint: "claude-code plan <command> -h lists that command's flags.",
}

// cmdPlan is the `plan` verb.
func cmdPlan(args []string) int {
	if code, ok := helpOrBare(planPage, args); ok {
		return code
	}
	switch args[0] {
	case "status":
		return cmdPlanStatus(args[1:])
	case "tick":
		return cmdPlanTick(args[1:])
	case "task":
		return cmdPlanTask(args[1:])
	case "ready":
		return cmdPlanReady(args[1:])
	case "assumptions":
		return planlint.RunAssumptions(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "goals":
		return planlint.RunGoals(args[1:], os.Stdin, os.Stdout, os.Stderr)
	default:
		return unknownSub("plan", args[0], planPage)
	}
}

func cmdPlanStatus(args []string) int {
	fs := helptext.NewFlagSet("plan status", helptext.Page{Usage: "claude-code plan status [flags]",
		About: "Show ticked/total tasks and live claims per plan of this repo."})
	all := fs.Bool("all", false, "every repo under the plans home, not just this repo's")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	summaries, err := plans.Status(plans.Dirs(*all)...)
	if err != nil {
		return helptext.Fail("plan status: %v", err)
	}
	if *format == helptext.JSON {
		if summaries == nil {
			summaries = []plans.Summary{}
		}
		return helptext.WriteJSONOrFail("plan status", summaries)
	}
	printPlanSummaries(summaries)
	return 0
}

func printPlanSummaries(summaries []plans.Summary) {
	if len(summaries) == 0 {
		fmt.Println("no plans")
		return
	}
	root := filepath.Join(paths.ConfigDir(), "plans")
	for _, s := range summaries {
		rel, err := filepath.Rel(root, s.Path)
		if err != nil {
			rel = s.Path
		}
		row := fmt.Sprintf("%d/%d  %s", s.Ticked, s.Total, filepath.ToSlash(rel))
		if s.Dropped > 0 {
			row = fmt.Sprintf("%d/%d (%d dropped)  %s", s.Ticked, s.Total, s.Dropped, filepath.ToSlash(rel))
		}
		fmt.Println(textutil.TruncateCells(row, 80, "…"))
		for _, c := range s.Claims {
			fmt.Printf("  %s\n", c)
		}
	}
}

// cmdPlanReady is `plan ready`: open tasks whose blockers closed and whose owns no live party claims.
func cmdPlanReady(args []string) int {
	fs := helptext.NewFlagSet("plan ready", helptext.Page{Usage: "claude-code plan ready [flags]",
		About: "List open tasks of this repo's active plans a leaf can take now, and the free dispatch slots."})
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("plan ready: %v", err)
	}
	_, main := paths.CheckoutOf(cwd)
	root := cmp.Or(main, cwd)
	slots := make(chan int, 1)
	go func() { slots <- freeSlots() }() // the registry read overlaps the plan and git reads
	rep, err := plans.Ready(plans.ReadyInput{Root: root, Claims: readyClaims})
	if err != nil {
		return helptext.Fail("plan ready: %v", err)
	}
	rep.Slots = <-slots
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("plan ready", rep)
	}
	printReady(rep)
	return 0
}

// freeSlots is the in-flight cap less the live agents, as doctor counts them.
func freeSlots() int { return max(router.CurrentLimits().Total-inflight.Count().Total, 0) }

// readyClaims is every live claim but this session's own edits; a leaf's counts while its last edit is fresh.
func readyClaims() map[string][]string {
	src := callerClaims()
	src.Leaf = func(c worktree.Claim) string {
		if c.Agent == "" || time.Since(time.Unix(c.TS, 0)) > inflight.Idle {
			return ""
		}
		return "leaf " + c.Agent[:min(8, len(c.Agent))]
	}
	hits, _ := worktree.LiveClaims(src) // an unreadable claims file drops only its own claims
	out := make(map[string][]string, len(hits))
	for p, hs := range hits {
		for _, h := range hs {
			out[p] = append(out[p], h.Who)
		}
	}
	return out
}

func printReady(rep plans.ReadyReport) {
	fmt.Printf("slots: %d free\n", rep.Slots)
	fmt.Printf("ready (%d):\n", len(rep.Ready))
	for _, t := range rep.Ready {
		fmt.Printf("  %s %s  leaf: %s  brief: %s\n", planSlug(t.Plan), t.ID, cmp.Or(t.Leaf, "-"), t.Brief)
		fmt.Printf("    owns: %s\n    verify: %s\n    next: %s\n", strings.Join(t.Owns, ", "), cmp.Or(t.Verify, "-"), t.Command)
	}
	fmt.Printf("not ready (%d):\n", len(rep.NotReady))
	for _, t := range rep.NotReady {
		fmt.Printf("  %s %s: %s\n", planSlug(t.Plan), t.ID, t.Reason)
	}
}

func planSlug(plan string) string { return strings.TrimSuffix(filepath.Base(plan), ".md") }

func cmdPlanTick(args []string) int {
	fs := helptext.NewFlagSet("plan tick", helptext.Page{Usage: "claude-code plan tick (--range <a>..<b> | --audit) [flags]",
		About: "Tick tasks named by commit trailers once on main; --audit also closes steps and reports."})
	rng := fs.String("range", "", "commits to tick, <a>..<b> as git rev-list reads it")
	plan := fs.String("plan", "", "plan file (default: matching recent or claimed plans)")
	dryRun := fs.Bool("dry-run", false, "print what would tick without writing the plan")
	audit := fs.Bool("audit", false, "audit this week's plans: tick, strike or report what git proves")
	all := fs.Bool("all", false, "with --audit: every age and repo home; cap 1000 writes")
	archive := fs.Bool("archive", false, "with --audit: archive closed plans after auditing")
	verify := fs.Bool("verify", false, "internal: verify --task ids on --sha, tick the green")
	sha := fs.String("sha", "", "with --verify: the landed commit")
	session := fs.String("session", "", "with --verify: the session that gets the result notice")
	var tasks []string
	fs.Func("task", "with --verify: a task id (repeatable)", func(v string) error { tasks = append(tasks, v); return nil })
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 {
		return helptext.FailUsage("plan tick: takes no positional arguments")
	}
	if (*all || *archive) && !*audit {
		return helptext.FailUsage("plan tick: --all and --archive require --audit")
	}
	if *audit && (*rng != "" || *plan != "" || *verify) {
		return helptext.FailUsage("plan tick: --audit takes neither --range, --plan nor --verify")
	}
	if *verify && (*rng != "" || *dryRun) {
		return helptext.FailUsage("plan tick: --verify takes neither --range nor --dry-run")
	}
	if *verify {
		return runTickVerify(*plan, *sha, tasks, *session)
	}
	if *rng == "" && !*audit {
		return helptext.FailUsage("plan tick: --range <a>..<b> is required")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	top, _ := paths.CheckoutOf(cwd)
	if top == "" {
		return helptext.Fail("plan tick: %s is not in a git checkout", cwd)
	}
	if *audit {
		return cmdPlanAudit(top, plans.AuditOpts{DryRun: *dryRun, All: *all}, *archive, *format)
	}
	return tickRange(top, *rng, *plan, *dryRun)
}

func tickRange(top, rng, plan string, dryRun bool) int {
	if plan != "" {
		resolved, err := plans.ResolvePlan(top, plan)
		if err != nil {
			return helptext.FailUsage("plan tick: %v", err)
		}
		plan = resolved
	}
	if err := plans.TickRange(context.Background(), top, plans.WorktreeName(top), rng, plan, plans.MirrorDests(), dryRun, os.Stdout); err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	return 0
}

// tickVerifyTimeout caps one detached verifier run.
const tickVerifyTimeout = 20 * time.Minute

// runTickVerify ticks each green task with sha and posts one notice per task; red leaves it open with the tail.
func runTickVerify(plan, sha string, ids []string, session string) int {
	if plan == "" || sha == "" || len(ids) == 0 {
		return helptext.FailUsage("plan tick: --verify needs --plan, --sha and --task")
	}
	top, err := os.Getwd()
	if err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	p, err := plans.Parse(plan)
	if err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	tasks := slices.DeleteFunc(slices.Clone(p.Tasks), func(t plans.Task) bool { return !slices.Contains(ids, t.ID) })
	ctx, cancel := context.WithTimeout(context.Background(), tickVerifyTimeout)
	defer cancel()
	checks, err := waves.VerifyCommit(ctx, top, sha, tasks)
	if err != nil {
		tickNotice(session, "plans: "+strings.Join(ids, ", ")+" not ticked: verifier failed: "+err.Error())
		return 1
	}
	code := tickGreen(session, top, sha, tasks, reportChecks(session, sha, tasks, checks))
	tickFollowUp(session, top, sha, checks, reportVerifyFailures(plan, top, sha, tasks, checks)) // after the ticks and their notices, and never changing the exit code
	return code
}

// Seams: a test swaps both; the real ones write the ledgers and launch lanes.
var (
	tickSweep   = docs.SweepAutofixSpend
	tickAutofix = docs.GateAutofix
)

// tickFollowUp records fix-leaf spend on every run, green or red, then repairs a red gate; notices only.
func tickFollowUp(session, top, sha string, checks map[string][]waves.TaskCheck, failureIDs []string) {
	lines := tickSweep(time.Now())
	lines = append(lines, tickAutofix(top, sha, redGateOutput(checks), failureIDs...)...)
	for _, l := range lines {
		tickNotice(session, l)
	}
}

// reportVerifyFailures broadcasts each task's confirmed failing check and returns the failure ids; errors are dropped.
func reportVerifyFailures(plan, top, sha string, tasks []plans.Task, checks map[string][]waves.TaskCheck) []string {
	var ids []string
	stem := strings.TrimSuffix(filepath.Base(plan), filepath.Ext(plan))
	for _, t := range tasks {
		i := slices.IndexFunc(checks[t.ID], func(c waves.TaskCheck) bool { return c.Exit != 0 && c.Skip == "" })
		if i < 0 {
			continue
		}
		c := checks[t.ID][i]
		f := worktree.Failure{Kind: worktree.FailPlanCheck, Subject: strings.Join(strings.Fields(stem+" "+t.ID+"@"+sha[:min(7, len(sha))]), " "), // ReportFailure collapses it too
			Summary: c.Line(), Log: c.Saved, Repo: top}
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), f)
		ids = append(ids, worktree.FailureID(f.Kind, f.Subject))
	}
	return ids
}

// tickReady is the automatic fill and ready-task lines a written tick frees; a seam so a test needs no live slots.
var tickReady = func(session, top string) []string {
	ready := plans.ReadyInput{Root: top, Slots: freeSlots(), Claims: readyClaims}
	return plans.FillNotice(plans.AutofillInput{ReadyInput: ready, Session: session}) // claims before it sends
}

// tickGreen ticks the green tasks on sha and posts their notices; 1 means the plan write failed.
func tickGreen(session, top, sha string, tasks []plans.Task, green map[string]bool) int {
	results, _, err := plans.TickCommit(top, sha, nil, verifiedOnly(tasks, green))
	ticked := false
	for _, r := range results {
		ticked = ticked || len(r.Ticked) > 0
		for _, l := range r.Lines(sha) {
			tickNotice(session, l)
		}
	}
	if ticked {
		for _, n := range tickReady(session, top) { // advisory only: the lines ride the same notice as the tick
			if n != "" {
				tickNotice(session, n)
			}
		}
	}
	if err != nil {
		tickNotice(session, "plans: not ticked: "+err.Error())
		return 1
	}
	return 0
}

// redGateOutput joins the saved output of each failed, unskipped check; all green is "".
func redGateOutput(checks map[string][]waves.TaskCheck) string {
	var out strings.Builder
	for _, id := range slices.Sorted(maps.Keys(checks)) {
		for _, c := range checks[id] {
			if c.Exit == 0 || c.Skip != "" || c.Saved == "" {
				continue
			}
			if b, err := os.ReadFile(c.Saved); err == nil {
				out.WriteString(strings.TrimSuffix(string(b), "\n"))
				out.WriteByte('\n')
			}
		}
	}
	return out.String()
}

// verifiedOnly admits green tasks of this plan; a same-id task in another plan differs in title or position.
func verifiedOnly(tasks []plans.Task, green map[string]bool) func(plans.Task) bool {
	return func(t plans.Task) bool {
		return green[t.ID] && slices.ContainsFunc(tasks, func(v plans.Task) bool {
			return v.ID == t.ID && v.Title == t.Title && v.Start == t.Start
		})
	}
}

// reportChecks posts one notice per task and returns the ids whose checks are all green.
func reportChecks(session, sha string, tasks []plans.Task, checks map[string][]waves.TaskCheck) map[string]bool {
	short := sha[:min(7, len(sha))]
	green := map[string]bool{}
	for _, t := range tasks {
		cs := checks[t.ID]
		if waves.AllOK(cs) {
			green[t.ID] = true
			tickNotice(session, "plans: "+t.ID+" verified on "+short+greenSummary(cs))
			continue
		}
		lines := make([]string, 0, len(cs))
		for _, c := range cs {
			lines = append(lines, c.Line())
		}
		tickNotice(session, "plans: "+t.ID+" not ticked on "+short+":\n"+strings.Join(lines, "\n"))
	}
	return green
}

// greenSummary is " (N s)" over the passed checks, plus the line of each skipped one.
func greenSummary(cs []waves.TaskCheck) string {
	var took time.Duration
	var skips []string
	for _, c := range cs {
		if c.Skip != "" {
			skips = append(skips, c.Line())
			continue
		}
		took += c.Took
	}
	return fmt.Sprintf(" (%.1f s)", took.Seconds()) + strings.Join(append([]string{""}, skips...), "; ")
}

// tickNotice prints line to the verifier log (stdout) and queues it for session's next prompt.
func tickNotice(session, line string) {
	fmt.Println(line)
	if session != "" {
		_ = worktree.Append(session, worktree.Notice{Schema: 1, TS: time.Now().Unix(), From: "plan-tick", Kind: "plan-tick", Text: line})
	}
}

// cmdPlanAudit runs plans.Audit and prints its report as text or one JSON object.
func cmdPlanAudit(top string, opts plans.AuditOpts, archive bool, format string) int {
	ctx := context.Background()
	rep, err := plans.Audit(ctx, top, opts)
	if err == nil && archive {
		var changes []plans.Change
		var findings []plans.Finding
		changes, findings, err = plans.Archive(ctx, top, opts.DryRun)
		rep.Changed, rep.Reported = append(rep.Changed, changes...), append(rep.Reported, findings...)
		if opts.DryRun {
			fmt.Fprintln(os.Stderr, "plan tick: note: the dry-run archive list omits plans this run's ticks would close")
		}
	}
	if err == nil && format == helptext.JSON {
		return helptext.WriteJSONOrFail("plan tick", rep)
	} else if err == nil {
		err = rep.WriteText(os.Stdout)
	}
	if err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	return 0
}

// cmdPlanTask is `plan task`: the by-hand tick, reopen and strike for what no commit can decide.
func cmdPlanTask(args []string) int {
	fs := helptext.NewFlagSet("plan task", helptext.Page{Usage: "claude-code plan task <id> --done|--reopen|--strike <why> [flags]",
		About: "Tick, reopen or strike one task by hand; quote an id with spaces: \"Phase 2.1\"."})
	done := fs.Bool("done", false, "tick the task manually, with or without owns:")
	reopen := fs.Bool("reopen", false, "untick the task and drop its sha (a revert); only a later commit naming it ticks it again")
	strike := fs.String("strike", "", "strike the task as superseded, with this reason")
	sha := fs.String("sha", "", "with --done: the commit to record")
	plan := fs.String("plan", "", "plan file (default: the one plan holding the id)")
	reason := fs.String("reason", "", "strike with this reason: "+strings.Join(plans.StrikeReasons, "|")+" (needs --evidence)")
	evidence := fs.String("evidence", "", "with --reason: the cite, decision:<id>, commit:<sha> or note:<path>")
	add, newTask := addFlags(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	id, one := oneID("", pos)
	byID := *reason != "" || *evidence != ""
	if !one || id == "" || taskModes(*done, *reopen, *strike != "", byID, *add) != 1 || (*sha != "" && !*done) {
		return helptext.FailUsage("plan task: give <id> and exactly one of --done, --reopen, --strike <why>, --reason with --evidence or --add")
	}
	if *add {
		return addTask(*plan, newTask(id))
	}
	path, err := taskPlan(id, *plan, *reopen)
	if err != nil {
		if *plan != "" {
			return helptext.FailUsage("plan task: %v", err)
		}
		return helptext.Fail("plan task: %v", err)
	}
	switch {
	case *done:
		_, err = plans.MarkDone(path, id, *sha)
	case *reopen:
		err = reopenTask(path, id)
	case byID:
		_, err = plans.StrikeWith(path, id, *reason, *evidence)
	default:
		err = plans.Strike(path, id, *strike)
	}
	if err != nil {
		return helptext.Fail("plan task: %v", err)
	}
	fmt.Printf("plans: %s updated in %s\n", id, filepath.Base(path))
	return 0
}

// leadingID splits a first argument that is not a flag off args.
func leadingID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// addFlags registers the --add flags and returns the flag and the builder of the task they describe.
func addFlags(fs *flag.FlagSet) (*bool, func(id string) plans.WaveTask) {
	add := fs.Bool("add", false, "append a new open task line, rendered from --title, --owns and the optional fields below")
	var t plans.WaveTask
	fs.StringVar(&t.Task, "title", "", "with --add: the task title")
	fs.StringVar(&t.Leaf, "leaf", "", "with --add: the leaf subagent_type")
	fs.StringVar(&t.Agent, "agent", "", "with --add: the requested agent")
	fs.StringVar(&t.Profile, "profile", "", "with --add: the profile")
	fs.StringVar(&t.Verify, "verify", "", "with --add: the verify command")
	blocked := fs.String("blocked-by", "", "with --add: comma-separated task ids that must be done first")
	owns := fs.String("owns", "", "with --add: comma-separated owned files")
	return add, func(id string) plans.WaveTask {
		t.ID, t.Owns, t.BlockedBy = id, textutil.SplitCSV(*owns), textutil.SplitCSV(*blocked)
		return t
	}
}

// addTask appends t to the plan named by --plan.
func addTask(plan string, t plans.WaveTask) int {
	if plan == "" {
		return helptext.FailUsage("plan task --add: pass --plan <file>")
	}
	cwd, _ := os.Getwd()
	top, _ := paths.CheckoutOf(cwd)
	path, err := plans.ResolvePlan(top, plan)
	if err == nil {
		err = plans.AddTask(path, t)
	}
	if err != nil {
		return helptext.Fail("plan task: %v", err)
	}
	fmt.Printf("plans: %s added to %s\n", t.ID, filepath.Base(path))
	return 0
}

// taskModes counts the chosen plan task actions.
func taskModes(on ...bool) int {
	return len(slices.DeleteFunc(on, func(b bool) bool { return !b }))
}

// reopenTask reopens id, marking the trunk of the plan's own repo, whatever the cwd.
func reopenTask(path, id string) error {
	cwd, _ := os.Getwd() // taskPlan already failed on an unreadable cwd
	top, _ := paths.CheckoutOf(cwd)
	root := plans.PlanRepo(path, top)
	if root == "" {
		fmt.Fprintf(os.Stderr, "plans: no checkout found for %s; reopened without a marker\n", filepath.Dir(path))
	}
	return plans.Reopen(root, path, id)
}

// taskPlan resolves an explicit plan or the one repo plan with a matching task.
func taskPlan(id, plan string, ticked bool) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	top, _ := paths.CheckoutOf(cwd)
	if plan != "" {
		return plans.ResolvePlan(top, plan)
	}
	if top == "" {
		return "", fmt.Errorf("%s is not in a git checkout; pass --plan", cwd)
	}
	found := plans.TaskPlans(plans.Home(top), plans.MirrorDests(), id, func(t plans.Task) bool {
		return !t.Dropped && t.Done == ticked
	})
	switch len(found) {
	case 0:
		var ids []string
		for _, o := range plans.OpenTasks(top) {
			ids = append(ids, o.ID)
		}
		return "", fmt.Errorf("no plan of this repo has a matching task %q; open ids: %s", id, strings.Join(ids, ", "))
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = filepath.Base(f)
	}
	return "", fmt.Errorf("task %q is in %d plans (%s); pick one with --plan <file>", id, len(found), strings.Join(names, ", "))
}

// planSpec returns the plan completion tree.
func planSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"status": {Flags: []helptext.Flag{{Name: "all", IsBool: true},
			helptext.CompletionFormatFlag()}},
		"ready":       {Flags: []helptext.Flag{helptext.CompletionFormatFlag()}},
		"assumptions": {Flags: []helptext.Flag{helptext.BoolFlag("json")}}, "goals": {Flags: []helptext.Flag{helptext.BoolFlag("json")}},
		"tick": {Flags: []helptext.Flag{{Name: "range"}, {Name: "plan"}, {Name: "dry-run", IsBool: true}, {Name: "audit", IsBool: true},
			{Name: "all", IsBool: true}, {Name: "archive", IsBool: true},
			{Name: "verify", IsBool: true}, {Name: "sha"}, {Name: "task"}, {Name: "session"}, helptext.CompletionFormatFlag()}},
		"task": {Flags: []helptext.Flag{helptext.BoolFlag("done"), helptext.BoolFlag("reopen"), {Name: "strike"}, {Name: "sha"}, {Name: "plan"}, {Name: "reason"}, {Name: "evidence"},
			helptext.BoolFlag("add"), {Name: "title"}, {Name: "leaf"}, {Name: "agent"}, {Name: "profile"}, {Name: "verify"}, {Name: "blocked-by"}, {Name: "owns"}}},
	}}
}
