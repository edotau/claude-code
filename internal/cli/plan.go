package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/review/planlint"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

var planPage = helptext.Page{
	Usage: "claude-code plan <command> [flags]",
	About: "Plan checklists (ticked/total, live claims) and plan detectors for vague steps and goals.",
	Commands: [][2]string{
		{"status", "progress per plan; --all spans every repo under the plans home"},
		{"tick", "tick tasks in --range <a>..<b>; --audit closes this week's plans"},
		{"task", "tick, reopen or strike one task by hand"},
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
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("", err)
	}
	summaries, err := plans.Status(plans.Dirs(*all)...)
	if err != nil {
		return helptext.Fail("plan status: %v", err)
	}
	if *format == helptext.JSON {
		if summaries == nil {
			summaries = []plans.Summary{}
		}
		if err := helptext.WriteJSON(os.Stdout, summaries); err != nil {
			return helptext.Fail("plan status: %v", err)
		}
		return 0
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

func cmdPlanTick(args []string) int {
	fs := helptext.NewFlagSet("plan tick", helptext.Page{Usage: "claude-code plan tick (--range <a>..<b> | --audit) [flags]",
		About: "Tick tasks named by commit trailers once on main; --audit also closes steps and reports."})
	rng := fs.String("range", "", "commits to tick, <a>..<b> as git rev-list reads it")
	plan := fs.String("plan", "", "plan file (default: matching recent or claimed plans)")
	dryRun := fs.Bool("dry-run", false, "print what would tick without writing the plan")
	audit := fs.Bool("audit", false, "audit this week's plans: tick, strike or report what git proves")
	verify := fs.Bool("verify", false, "internal: verify --task ids on --sha, tick the green")
	sha := fs.String("sha", "", "with --verify: the landed commit")
	session := fs.String("session", "", "with --verify: the session that gets the result notice")
	var tasks []string
	fs.Func("task", "with --verify: a task id (repeatable)", func(v string) error { tasks = append(tasks, v); return nil })
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *verify {
		return runTickVerify(*plan, *sha, tasks, *session)
	}
	if *audit && (*rng != "" || *plan != "") {
		return helptext.FailUsage("plan tick: --audit takes neither --range nor --plan")
	}
	if *rng == "" && !*audit {
		return helptext.Fail("plan tick: --range <a>..<b> is required")
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("", err)
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
		return cmdPlanAudit(top, *dryRun, *format)
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
	green := reportChecks(session, sha, tasks, checks)
	results, _, err := plans.TickCommit(top, sha, nil, verifiedOnly(tasks, green))
	for _, r := range results {
		for _, l := range r.Lines(sha) {
			tickNotice(session, l)
		}
	}
	if err != nil {
		tickNotice(session, "plans: not ticked: "+err.Error())
		return 1
	}
	return 0
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
func cmdPlanAudit(top string, dryRun bool, format string) int {
	rep, err := plans.Audit(context.Background(), top, dryRun)
	if err == nil && format == helptext.JSON {
		err = helptext.WriteJSON(os.Stdout, rep)
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
	reopen := fs.Bool("reopen", false, "untick the task and drop its sha (a revert)")
	strike := fs.String("strike", "", "strike the task as superseded, with this reason")
	sha := fs.String("sha", "", "with --done: the commit to record")
	plan := fs.String("plan", "", "plan file (default: the one plan holding the id)")
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	modes := 0
	for _, on := range []bool{*done, *reopen, *strike != ""} {
		if on {
			modes++
		}
	}
	if id == "" || modes != 1 || fs.NArg() > 1 || (*sha != "" && !*done) {
		return helptext.Fail("plan task: give <id> and exactly one of --done, --reopen, --strike <why>")
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
		err = plans.MarkDone(path, id, *sha)
	case *reopen:
		err = plans.Reopen(path, id)
	default:
		err = plans.Strike(path, id, *strike)
	}
	if err != nil {
		return helptext.Fail("plan task: %v", err)
	}
	fmt.Printf("plans: %s updated in %s\n", id, filepath.Base(path))
	return 0
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
		return "", fmt.Errorf("no plan of this repo has a matching task %q", id)
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
		"assumptions": {Flags: []helptext.Flag{helptext.BoolFlag("json")}}, "goals": {Flags: []helptext.Flag{helptext.BoolFlag("json")}},
		"tick": {Flags: []helptext.Flag{{Name: "range"}, {Name: "plan"}, {Name: "dry-run", IsBool: true}, {Name: "audit", IsBool: true},
			{Name: "verify", IsBool: true}, {Name: "sha"}, {Name: "task"}, {Name: "session"}, helptext.CompletionFormatFlag()}},
		"task": {Flags: []helptext.Flag{helptext.BoolFlag("done"), helptext.BoolFlag("reopen"), {Name: "strike"}, {Name: "sha"}, {Name: "plan"}}},
	}}
}
