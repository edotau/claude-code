package plans

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	auditWriteCap = 20
	auditLogKeep  = 1000
	auditIDCells  = 48
)

// processPrefixes open the template's fixed closing steps (docs/templates/plan.md).
var processPrefixes = []string{"Live check", "Scoped tests", "Review", "Full suite", "Commit", "Handoff"}

// Change is one plan edit the audit made (or would make) and the command that undoes it.
type Change struct {
	Time     time.Time `json:"time"`
	Plan     string    `json:"plan"`
	ID       string    `json:"id"`
	Action   string    `json:"action"`
	Rule     string    `json:"rule"`
	Evidence string    `json:"evidence"`
	Reopen   string    `json:"reopen"`
}

// Finding is something the audit reports and never fixes.
type Finding struct {
	Plan     string `json:"plan"`
	ID       string `json:"id"`
	Problem  string `json:"problem"`
	Evidence string `json:"evidence"`
}

// AuditReport is one audit run.
type AuditReport struct {
	DryRun    bool      `json:"dry_run"`
	Plans     int       `json:"plans"`
	Changed   []Change  `json:"changed"`
	Reported  []Finding `json:"reported"`
	Remaining int       `json:"remaining"`
}

// reach answers whether a commit is in the history of one ref.
type reach struct {
	ctx    context.Context
	root   string
	ref    string
	recent []string // newest first
}

func newReach(ctx context.Context, root, ref, since string) (*reach, error) {
	out, err := proc.RunGitInherit(ctx, root, "rev-list", ref, "--since="+since, "--")
	if err != nil {
		return nil, err
	}
	return &reach{ctx, root, ref, strings.Fields(out)}, nil
}

// index is sha's position among the recent commits of the ref (0 newest), -1 when absent.
func (r *reach) index(sha string) int {
	sha = strings.ToLower(sha)
	return slices.IndexFunc(r.recent, func(full string) bool { return strings.HasPrefix(full, sha) })
}

func (r *reach) has(sha string) bool {
	if r.index(sha) >= 0 {
		return true
	}
	_, err := proc.RunGitInherit(r.ctx, r.root, "merge-base", "--is-ancestor", sha, r.ref)
	return err == nil
}

type auditRun struct {
	ctx           context.Context
	root          string
	dry           bool
	trunk, landed *reach
	rep           AuditReport
	writes        int
}

// Audit ticks and strikes what git and the plan prove, and reports the rest; it never guesses.
func Audit(ctx context.Context, root string, dryRun bool) (AuditReport, error) {
	a := &auditRun{ctx: ctx, root: root, dry: dryRun, rep: AuditReport{DryRun: dryRun, Changed: []Change{}, Reported: []Finding{}}}
	trunk, _, _, err := trunkBranch(ctx, root)
	if err != nil {
		return a.rep, err
	}
	dated, since := recentPlans(root)
	a.rep.Plans = len(dated)
	if len(dated) == 0 {
		return a.rep, nil
	}
	if err := a.openRefs(trunk, since); err != nil {
		return a.rep, err
	}
	commits, err := a.trailerCommits(since, "refs/heads/"+trunk)
	if err != nil {
		return a.rep, err
	}
	for _, d := range dated {
		tasks := slices.Clone(d.plan.Tasks)
		a.check(d.path, tasks)
		if err := a.trailers(d.path, tasks, commits); err != nil {
			return a.rep, err
		}
		if err := a.process(d.path, tasks); err != nil {
			return a.rep, err
		}
	}
	return a.rep, nil
}

// recentPlans lists the dated plans within recentDays, oldest first, and the day before the oldest one.
func recentPlans(root string) (dated []datedPlan, since string) {
	home := Home(root)
	entries, _ := os.ReadDir(home) // no plans home is no plans
	cutoff := now().AddDate(0, 0, -recentDays).Format(time.DateOnly)
	for _, d := range loadDatedPlans(entries, home, MirrorDests()) {
		if filepath.Base(d.path)[:10] >= cutoff {
			dated = append(dated, d)
		}
	}
	if len(dated) == 0 {
		return nil, ""
	}
	oldest, _ := time.Parse(time.DateOnly, filepath.Base(dated[0].path)[:10]) // datedName guarantees the shape
	return dated, oldest.AddDate(0, 0, -1).Format(time.DateOnly)
}

// openRefs reads trunk's recent history and, when origin has the trunk, the history a tick must be reachable from.
func (a *auditRun) openRefs(trunk, since string) (err error) {
	if a.trunk, err = newReach(a.ctx, a.root, "refs/heads/"+trunk, since); err != nil {
		return err
	}
	a.landed = a.trunk
	remote := "refs/remotes/origin/" + trunk
	if _, verr := proc.RunGitInherit(a.ctx, a.root, "rev-parse", "--verify", "--quiet", remote); verr != nil {
		return nil // no remote trunk: local trunk is the whole truth
	}
	a.landed, err = newReach(a.ctx, a.root, remote, since)
	return err
}

// trailerCommits lists trunk commits since a date whose message may hold a Task trailer.
func (a *auditRun) trailerCommits(since, ref string) ([]Commit, error) {
	out, err := proc.RunGitInherit(a.ctx, a.root, "log", ref, "--since="+since, "--fixed-strings", "--grep=Task: ", "--format=%x01%H%x00%B", "--")
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for rec := range strings.SplitSeq(out, "\x01") {
		if sha, msg, ok := strings.Cut(rec, "\x00"); ok {
			commits = append(commits, Commit{Sha: sha, Message: msg})
		}
	}
	return commits, nil
}

// apply counts a write against the cap, does it unless dry, and logs it; false means it was not made.
func (a *auditRun) apply(c Change, write func() error) (bool, error) {
	if a.writes >= auditWriteCap {
		a.rep.Remaining++
		return false, nil
	}
	a.writes++
	c.Time, c.Reopen = now().UTC(), reopenCmd(c.Plan, c.ID)
	if !a.dry {
		if err := write(); err != nil {
			return false, err
		}
		if err := appendAuditLog(c); err != nil {
			return false, err
		}
	}
	a.rep.Changed = append(a.rep.Changed, c)
	return true, nil
}

func (a *auditRun) report(plan, id, problem, evidence string) {
	a.rep.Reported = append(a.rep.Reported, Finding{plan, id, problem, evidence})
}

// tick ticks task t (by id, else title) with sha, mirroring the edit in tasks so later rules see it.
func (a *auditRun) tick(plan string, t *Task, sha, rule string) error {
	id := cmp.Or(t.ID, t.Title)
	ok, err := a.apply(Change{Plan: plan, ID: id, Action: "tick", Rule: rule, Evidence: sha}, func() error { return MarkDone(plan, id, sha) })
	if ok {
		t.Done, t.Sha = true, sha
	}
	return err
}

// trailers ticks open tasks a landed Task trailer names and reports trailers naming no task or an unpushed commit.
func (a *auditRun) trailers(plan string, tasks []Task, commits []Commit) error {
	slug := strings.TrimSuffix(filepath.Base(plan), ".md")
	for _, c := range commits {
		for _, v := range taskTrailerValues(c.Message) {
			f := strings.Fields(v)
			if len(f) < 2 || f[0] != slug {
				continue
			}
			id, short := strings.Join(f[1:], " "), c.Sha[:7]
			i := slices.IndexFunc(tasks, func(t Task) bool { return t.ID == id })
			switch {
			case i < 0:
				a.report(slug, id, "trailer names a task the plan does not have", short)
			case !open(tasks[i]) || countID(tasks, id) > 1:
			case !a.landed.has(c.Sha):
				a.report(slug, id, "landed locally, not pushed", short)
			default:
				if err := a.tick(plan, &tasks[i], short, "trailer"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func countID(tasks []Task, id string) int {
	return len(slices.DeleteFunc(slices.Clone(tasks), func(t Task) bool { return t.ID != id }))
}

// check reports malformed or suspect lines; it never edits.
func (a *auditRun) check(plan string, tasks []Task) {
	slug := strings.TrimSuffix(filepath.Base(plan), ".md")
	lines := map[string][]string{}
	for _, t := range tasks {
		if t.ID != "" {
			lines[t.ID] = append(lines[t.ID], strconv.Itoa(t.Start+1))
		}
	}
	for _, t := range tasks {
		if problem, evidence := a.suspect(t, lines[t.ID]); problem != "" {
			a.report(slug, cmp.Or(t.ID, t.Title), problem, evidence)
		}
	}
}

// suspect is the first malformation of t; same lists every line holding t's id.
func (a *auditRun) suspect(t Task, same []string) (problem, evidence string) {
	switch {
	case len(same) > 1 && same[0] == strconv.Itoa(t.Start+1):
		return "duplicate id", "lines " + strings.Join(same, ", ")
	case t.Done && t.Dropped:
		return "ticked and struck", fmt.Sprint("line ", t.Start+1)
	case open(t) && t.Sha != "":
		return "open task carries a sha", t.Sha
	case t.Done && t.Claim != "":
		return "ticked task still holds a claim", t.Claim
	case t.Done && t.Sha != "" && !a.trunk.has(t.Sha):
		return "tick sha is not on trunk", t.Sha
	}
	return "", ""
}

func isProcessStep(t Task) bool {
	return open(t) && t.ID == "" && len(t.Owns) == 0 &&
		slices.ContainsFunc(processPrefixes, func(p string) bool { return strings.HasPrefix(t.Title, p) })
}

// process closes the template's closing steps of a plan dated before today once its build tasks are closed for good.
func (a *auditRun) process(plan string, tasks []Task) error {
	slug := strings.TrimSuffix(filepath.Base(plan), ".md")
	if slug[:10] >= now().Format(time.DateOnly) {
		return nil
	}
	var steps []int
	var build []Task
	for i, t := range tasks {
		if t.ID != "" {
			build = append(build, t)
		} else if isProcessStep(t) {
			steps = append(steps, i)
		}
	}
	if len(steps) == 0 || len(build) == 0 {
		return nil
	}
	shas, allStruck, ok := a.landings(slug, build)
	if !ok {
		return nil
	}
	sha := ""
	if !allStruck {
		sha = a.newest(shas)
	}
	for _, i := range steps {
		if err := a.closeStep(plan, tasks, i, allStruck, sha); err != nil {
			return err
		}
	}
	return nil
}

// landings is the sha of every ticked build task and whether all were struck; ok is false when the plan is not closed.
func (a *auditRun) landings(slug string, build []Task) (shas []string, allStruck, ok bool) {
	struck := 0
	for _, t := range build {
		switch {
		case open(t) || (t.Done && t.Dropped):
			return nil, false, false
		case t.Dropped:
			struck++
		case t.Sha == "" || !a.landed.has(t.Sha):
			a.report(slug, "process steps", "build task landing is not on the remote trunk", cmp.Or(t.ID, t.Title)+" "+cmp.Or(t.Sha, "no sha"))
			return nil, false, false
		default:
			shas = append(shas, t.Sha)
		}
	}
	return shas, struck == len(build), true
}

// closeStep strikes (every build task struck) or ticks step i; a title another task also starts with is left alone.
func (a *auditRun) closeStep(plan string, tasks []Task, i int, allStruck bool, sha string) error {
	t := &tasks[i]
	if found, ok := findTask(Plan{Tasks: tasks}, t.Title); !ok || found.Start != t.Start {
		a.report(strings.TrimSuffix(filepath.Base(plan), ".md"), t.Title, "process step title is not unique", "left open")
		return nil
	}
	if !allStruck {
		return a.tick(plan, t, sha, "process")
	}
	const why = "plan closed: every build task struck"
	c := Change{Plan: plan, ID: t.Title, Action: "strike", Rule: "process", Evidence: why}
	_, err := a.apply(c, func() error { return Strike(plan, t.Title, why) })
	return err
}

// newest is the sha of shas that sits nearest trunk's tip.
func (a *auditRun) newest(shas []string) string {
	best, at := shas[0], len(a.landed.recent)
	for _, s := range shas {
		if i := a.landed.index(s); i >= 0 && i < at {
			best, at = s, i
		}
	}
	return best
}

func auditLogFile() string { return filepath.Join(paths.StateDir(), "memory", "plans-audit.jsonl") }

// appendAuditLog adds c's line to the log under its lock, keeping the newest auditLogKeep lines.
func appendAuditLog(c Change) error {
	line, err := json.Marshal(c)
	if err != nil {
		return err
	}
	file := auditLogFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(file, func() error {
		old, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		lines := append(bytes.Split(bytes.TrimSpace(old), []byte("\n")), line)
		lines = slices.DeleteFunc(lines, func(l []byte) bool { return len(l) == 0 })
		lines = lines[max(0, len(lines)-auditLogKeep):]
		return paths.AtomicWrite(file, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600)
	})
}

// WriteText prints changed rows, then reported rows, then one summary line.
func (r AuditReport) WriteText(w io.Writer) error {
	verb := map[string]string{"tick": "ticked", "strike": "struck"}
	if r.DryRun {
		verb = map[string]string{"tick": "would tick", "strike": "would strike"}
	}
	row := func(plan, id, what, evidence string) error {
		_, err := fmt.Fprintf(w, "%s: %s: %s: %s\n", plan, textutil.CapRunes(id, auditIDCells, "…"), what, evidence)
		return err
	}
	ticks, strikes := 0, 0
	for _, c := range r.Changed {
		if c.Action == "tick" {
			ticks++
		} else {
			strikes++
		}
		if err := row(strings.TrimSuffix(filepath.Base(c.Plan), ".md"), c.ID, verb[c.Action], c.Evidence); err != nil {
			return err
		}
	}
	for _, f := range r.Reported {
		if err := row(f.Plan, f.ID, f.Problem, f.Evidence); err != nil {
			return err
		}
	}
	tail := ""
	if r.Remaining > 0 {
		tail = fmt.Sprintf(", %d more over the cap of %d writes", r.Remaining, auditWriteCap)
	}
	_, err := fmt.Fprintf(w, "plans: audit %d plans: %s %d, %s %d, reported %d%s\n", r.Plans, verb["tick"], ticks, verb["strike"], strikes, len(r.Reported), tail)
	return err
}
