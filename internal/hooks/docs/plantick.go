package docs

import (
	"bytes"
	"cmp"
	"context"

	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

var (
	gitDirArg  = regexp.MustCompile(`\bgit\s+(?:-\S+\s+)*?-C\s+("[^"]+"|'[^']+'|\S+)`)
	cmdSegment = regexp.MustCompile(`&&|\|\||[;|\n]`)
	leadingCD  = regexp.MustCompile(`^\s*cd\s+("[^"]+"|'[^']+'|[^\s&;]+)\s*&&`)
)

// Tick is the PostToolUse(Bash) plan-tick hook: a landed `git commit` ticks the active plan's covered tasks.
func Tick(in *core.Input) core.Result {
	cmd := in.ToolCommand()
	if in.ToolName != "Bash" || !core.GitCommitCmd.MatchString(cmd) {
		return core.Result{}
	}
	out := in.Nested("tool_response", "stdout")
	if out == "" {
		out = in.ToolResponseText()
	}
	var blocks []string
	var stderr bytes.Buffer
	seen := map[string]bool{}
	for _, m := range core.CommitLine.FindAllStringSubmatch(out, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			blocks = append(blocks, tickCommit(in, cmd, m[1], &stderr)...)
		}
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	res.Context = core.JoinContext(blocks...)
	return res
}

// tickCommit ticks the active plan's tasks covered by one landed commit and returns the context blocks.
func tickCommit(in *core.Input, cmd, sha string, stderr io.Writer) []string {
	ctx, cancel := core.HookContext("docs-plan-tick")
	defer cancel()
	dir := commitRepoDir(ctx, in.CWD, cmd, sha)
	if dir == "" {
		return nil
	}
	top := memstore.GitToplevel(dir)
	worktree := plans.WorktreeName(top)
	files, err := plans.CommitFiles(ctx, dir, sha)
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return nil
	}
	plan, tied := plans.Select(plans.Home(top), worktree, plans.MirrorDests(), []plans.Commit{{Sha: sha, Files: files}})
	if plan == "" {
		if len(tied) > 0 {
			fmt.Fprintf(stderr, "plan-tick: %d plans match %s equally; nothing annotated\n", len(tied), sha)
		}
		return nil
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	ticked, partial, err := plans.Tick(plan, short, files)
	if err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return nil
	}
	base := strings.TrimSuffix(filepath.Base(plan), ".md")
	if len(partial) > 0 {
		recordPartials(in.SessionID, base, plan, partial, stderr)
	}
	var blocks []string
	if len(ticked) > 0 {
		blocks = append(blocks, "plans: ticked "+strings.Join(ticked, "; ")+" ("+short+") in "+base)
	}
	if len(partial) > 0 {
		blocks = append(blocks, "plans: partly landed "+strings.Join(partial, "; ")+" in "+base)
	}
	return blocks
}

// commitRepoDir is the candidate dir (commit -C args, leading cd, cwd) whose HEAD is sha, else the first whose repo holds it.
func commitRepoDir(ctx context.Context, cwd, cmd, sha string) string {
	var holders []string
	for _, d := range commitCandidates(cwd, cmd) {
		if strings.HasPrefix(d, "~/") {
			d = paths.ExpandHome(d)
		}
		if d = resolveCWDPath(d, cwd); d == "" {
			continue
		}
		if _, err := proc.RunGit(ctx, d, "cat-file", "-e", sha+"^{commit}"); err != nil {
			continue
		}
		head, err := proc.RunGit(ctx, d, "rev-parse", "HEAD")
		if err == nil && strings.HasPrefix(strings.TrimSpace(head), sha) {
			return d // linked worktrees share objects; the one whose HEAD is the commit made it
		}
		holders = append(holders, d)
	}
	if len(holders) > 0 {
		return holders[0]
	}
	return ""
}

// commitCandidates lists the dirs a commit command may have run in: -C args, a leading cd, then cwd.
func commitCandidates(cwd, cmd string) []string {
	var cands []string
	for _, seg := range cmdSegment.Split(cmd, -1) { // only commit invocations name a candidate repo
		if !core.GitCommitCmd.MatchString(seg) {
			continue
		}
		if m := gitDirArg.FindStringSubmatch(seg); m != nil {
			cands = append(cands, strings.Trim(m[1], `"'`))
		}
	}
	if m := leadingCD.FindStringSubmatch(cmd); m != nil {
		cands = append(cands, strings.Trim(m[1], `"'`))
	}
	return append(cands, cwd)
}

// recordPartials appends "<path>\\t<plan>: <task>" lines for the Stop reminder.
func recordPartials(session, plan, planPath string, tasks []string, stderr io.Writer) {
	if err := os.MkdirAll(plans.ScratchDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
		return
	}
	var b strings.Builder
	for _, t := range tasks {
		fmt.Fprintf(&b, "%s\t%s: %s\n", planPath, plan, t)
	}
	file := partialFile(session)
	if err := paths.AppendLine(file, []byte(b.String())); err != nil {
		fmt.Fprintf(stderr, "plan-tick: %v\n", err)
	}
}

var (
	briefTaskRe = regexp.MustCompile(`(?mi)^[\s>*-]*Task:\s*(\S.*?)\s*$`)
	nonWordRe   = regexp.MustCompile(`[^\p{L}\p{N}.]+`)
)

// ClaimPlanTask claims the active plan's task a spawn names (a `Task:` brief line, else a unique checklist title
// the description matches); no confident match, no plan or any error claims nothing.
func ClaimPlanTask(in *core.Input, id string) {
	if !core.SpawnTools[in.ToolName] || id == "" {
		return
	}
	plan, worktreeName, ok := activePlan(in.CWD)
	if !ok {
		return
	}
	parsed, err := plans.Parse(plan)
	if err != nil {
		return
	}
	task, ok := matchBriefTask(parsed, in.Nested("tool_input", "prompt"), in.Nested("tool_input", "description"))
	if !ok || task.Claim != "" { // the locked ClaimFree below re-checks
		return
	}
	agent := cmp.Or(in.Nested("tool_input", "subagent_type"), in.ToolName)
	text := fmt.Sprintf("working: %s %s · worktree %s · since %s", agent, id, cmp.Or(worktreeName, "main"), time.Now().Format("15:04"))
	if plans.ClaimFree(plan, taskKey(task), text) != nil || worktreeName == "" {
		return
	}
	_, _ = worktree.SetLedgerTask(in.CWD, taskKey(task)+" in "+strings.TrimSuffix(filepath.Base(plan), ".md"), time.Now())
}

// ClearPlanClaim removes the claim a stopping agent's spawn wrote, found by its tool_use_id in the claim text.
func ClearPlanClaim(cwd, id string) {
	if id == "" {
		return
	}
	plan, _, ok := activePlan(cwd)
	if !ok {
		return
	}
	parsed, err := plans.Parse(plan)
	if err != nil {
		return
	}
	for _, t := range parsed.Tasks {
		if strings.Contains(t.Claim, " "+id+" ") {
			_ = plans.ClearClaimOwned(plan, taskKey(t), " "+id+" ")
		}
	}
}

// activePlan is cwd's repo plan with an open owned task, plus the linked worktree's name ("" in the main checkout).
func activePlan(cwd string) (plan, worktreeName string, ok bool) {
	top := memstore.GitToplevel(cwd)
	if top == "" {
		return "", "", false
	}
	worktreeName = plans.WorktreeName(top)
	plan, ok = plans.Active(plans.Home(top), worktreeName, plans.MirrorDests())
	return plan, worktreeName, ok
}

func taskKey(t plans.Task) string { return cmp.Or(t.ID, t.Title) }

func normTask(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "phase ")
	return strings.Trim(nonWordRe.ReplaceAllString(s, " "), " .")
}

// matchBriefTask returns the one open task a `Task:` line names by id, else the one a description matches by title.
func matchBriefTask(p plans.Plan, brief, description string) (plans.Task, bool) {
	if m := briefTaskRe.FindStringSubmatch(brief); m != nil {
		return matchTaskByID(p, normTask(m[1]))
	}
	return matchTaskByTitle(p, normTask(description))
}

func matchTaskByID(p plans.Plan, want string) (plans.Task, bool) {
	var hit []plans.Task
	for _, t := range p.Tasks {
		if !t.Done && want != "" && (normTask(t.ID) == want || normTask(t.Title) == want) {
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
		if title := normTask(t.Title); !t.Done && (title == want || strings.Contains(title, want)) {
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
func ClearStoppedClaim(in *core.Input, dirs []string, id string) {
	for _, d := range dirs {
		if tu := inflight.ToolUseID(d, id); tu != "" {
			ClearPlanClaim(in.CWD, tu)
			return
		}
	}
}
