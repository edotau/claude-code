package docs

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/markdown"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// Save files an approved ExitPlanMode plan as <config>/plans/[<repo>/]<date>-<title>.md (plans/ is gitignored).
// PostToolUse only fires on approval; prefer approved input text, then the response, then their scratch files.
// When the approved plan traces back to an undated draft the plan-mirror hook already filed, its recorded
// destination is reused (the date stays the draft's creation date) instead of minting a new one for today.
func Save(in *core.Input, stderr io.Writer) int {
	responsePath := in.Nested("tool_response", "filePath")
	inputPath := in.Nested("tool_input", "planFilePath")
	plan := approvedPlanBytes(in, responsePath, inputPath, stderr)
	if len(bytes.TrimSpace(plan)) == 0 {
		return core.ExitProceed
	}
	title := planTitle(plan) // from the approved text, before the checklist adds its own heading
	approvedText := plan     // snapshot before withTasks, to compare against the draft's current bytes below
	plan = withTasks(plan)

	draftPath := core.AnchorPath(cmp.Or(responsePath, inputPath), in.CWD)
	dest, ok := "", false
	if draftPath != "" {
		dest, ok = plans.ReadRecordedDest(filepath.Base(draftPath))
	}
	fresh := true
	if !ok {
		dest, fresh = planDestination(in.CWD, title, plan, true)
	}
	if !fresh {
		retireDraft(draftPath, approvedText, stderr) // the dated file already holds this plan
		return core.ExitProceed
	}
	if !writeVerifiedPlan(dest, plan, stderr) {
		return core.ExitProceed
	}
	audit.Log(audit.PlanSaved, "save-plan", dest)
	retireDraft(draftPath, approvedText, stderr)
	return core.ExitProceed
}

// approvedPlanBytes prefers the approved plan text, then falls back to the plan scratch files.
func approvedPlanBytes(in *core.Input, responsePath, inputPath string, stderr io.Writer) []byte {
	plan := []byte(in.Nested("tool_input", "plan"))
	if len(plan) == 0 {
		plan = []byte(in.Nested("tool_response", "plan"))
	}
	for _, path := range []string{responsePath, inputPath} {
		if len(plan) != 0 {
			break
		}
		if path != "" {
			var err error
			plan, err = os.ReadFile(core.AnchorPath(path, in.CWD))
			if err != nil {
				fmt.Fprintf(stderr, "save-plan: %v\n", err)
			}
		}
	}
	return plan
}

// writeVerifiedPlan writes plan to dest and re-reads it; it reports false (after logging) on any failure.
func writeVerifiedPlan(dest string, plan []byte, stderr io.Writer) bool {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
		return false
	}
	if err := plans.WriteFile(dest, plan); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
		dropEmptyClaim(dest)
		return false
	}
	// Verify the dated file actually holds the plan before touching the draft it was mirrored from.
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, plan) {
		fmt.Fprintf(stderr, "save-plan: verification failed for %s\n", dest)
		dropEmptyClaim(dest)
		return false
	}
	return true
}

// dropEmptyClaim removes the 0-byte placeholder planDestination claimed, so a retry reuses the name.
func dropEmptyClaim(dest string) {
	if fi, err := os.Stat(dest); err == nil && fi.Size() == 0 {
		if err := os.Remove(dest); err != nil {
			fmt.Fprintf(os.Stderr, "docs-plan: drop empty claim %s: %v\n", dest, err)
		}
	}
}

// retireDraft removes the mirrored draft and its .dest record once the plan is filed; an edited draft is kept.
func retireDraft(draftPath string, approvedText []byte, stderr io.Writer) {
	if draftPath == "" || !isUndatedDraft(draftPath) {
		return
	}
	record := plans.DestRecordPath(filepath.Base(draftPath))
	removeRecord := func() { dropRecord(record, stderr) }
	switch current, err := os.ReadFile(draftPath); {
	case os.IsNotExist(err):
		removeRecord()
	case err != nil:
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
	case !bytes.Equal(current, approvedText):
		fmt.Fprintf(stderr, "save-plan: draft %s was edited after approval; keeping it\n", draftPath)
		removeRecord()
	default:
		if err := os.Remove(draftPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "save-plan: %v\n", err)
		}
		removeRecord()
	}
}

// dropRecord removes a draft's .dest record, else truncates it, so Mirror cannot overwrite the approved dated plan.
func dropRecord(record string, stderr io.Writer) {
	err := os.Remove(record)
	if err == nil || os.IsNotExist(err) {
		return
	}
	fmt.Fprintf(stderr, "save-plan: %v; emptying %s so Mirror cannot reuse it\n", err, record)
	if err := os.WriteFile(record, nil, 0o600); err != nil {
		fmt.Fprintf(stderr, "save-plan: could not empty %s: %v\n", record, err)
	}
}

// planHeading is a heading's level and text from the plan AST; a "#" in a code fence or front matter never yields one.
type planHeading struct {
	level int
	text  string
}

// planHeadings walks body's parsed markdown AST for every heading, in document order.
func planHeadings(body []byte) []planHeading {
	doc := markdown.Parse(body)
	var out []planHeading
	_ = mdast.Walk(doc.AST, func(n mdast.Node, entering bool) (mdast.WalkStatus, error) { // walker never errors
		if entering {
			if h, ok := n.(*mdast.Heading); ok {
				out = append(out, planHeading{level: h.Level, text: string(h.Lines().Value(doc.Body))})
			}
		}
		return mdast.WalkContinue, nil
	})
	return out
}

// planSlugLine matches the template's "Suggested slug: `name`" line (backticks optional).
var planSlugLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("(?im)^(?:suggested )?slug:\\s*`?([a-z0-9][a-z0-9-]*)`?\\s*$")
})

// planTitle prefers an explicit slug line, else slugs the first heading minus a redundant "Plan:" lead-in.
func planTitle(plan []byte) string {
	if m := planSlugLine().FindSubmatch(plan); m != nil {
		return paths.Slug(string(m[1]), "-", 60, "plan")
	}
	headings := planHeadings(plan)
	if len(headings) == 0 {
		return "plan"
	}
	h := strings.TrimSpace(headings[0].text)
	if strings.HasPrefix(strings.ToLower(h), "plan:") {
		h = h[len("plan:"):]
	}
	return paths.Slug(h, "", 60, "plan")
}

// draftTarget is tool_input.file_path anchored to cwd; ok reports it names an undated draft.
func draftTarget(in *core.Input) (path string, ok bool) {
	path = core.AnchorPath(in.Nested("tool_input", "file_path"), in.CWD)
	return path, isUndatedDraft(path)
}

// planDestination picks <date>-<title>.md, then -2.md, … under the repo's plans dir; adopt reuses a
// candidate already holding content (fresh=false) — save-plan adopts, a mirror never takes a file it did not create.
func planDestination(cwd, title string, content []byte, adopt bool) (dest string, fresh bool) {
	dir := plans.Home(planRepoRoot(cwd))
	base := time.Now().Format("2006-01-02") + "-" + title
	for n := 1; ; n++ {
		name := base + ".md"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.md", base, n)
		}
		p := filepath.Join(dir, name)
		existing, err := os.ReadFile(p)
		if err != nil {
			if !os.IsNotExist(err) || claimPlanFile(p) {
				return p, true
			}
			continue
		}
		if adopt && bytes.Equal(existing, content) {
			return p, false
		}
	}
}

// planRepoRoot is cwd's git root; anything under the config dir (memory/<repo> is its own git repo) is the harness.
func planRepoRoot(cwd string) string {
	if cfg := paths.ConfigDir(); paths.Within(paths.CanonDeep(cwd), paths.Canon(cfg)) {
		return cfg
	}
	return cmp.Or(memory.GitToplevel(cwd), cwd)
}

// claimPlanFile reserves p with an O_EXCL create; false means a peer claimed it first.
func claimPlanFile(p string) bool {
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return true
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return !os.IsExist(err)
	}
	_ = f.Close() // the O_EXCL create already won the claim; a close error must not lose it
	return true
}

// taskHeadingWord picks the plan headings that become checklist items: phases, steps, parts and tasks.
var taskHeadingWord = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(?:Phase|Step|Part|Task|Stage|Milestone)\b`)
})

// planCloseout is the completion tail every plan's checklist ends with.
var planCloseout = []string{
	"Live check on the real surface",
	"Scoped tests for the touched packages",
	"Review (`claude-code codex review --adversarial --base <branch>`)",
	"Full suite once",
	"Commit, `make install`, push per task (named files only)",
	"Handoff / `claude-code memory update`",
}

// withTasks appends a `## Tasks` checklist when the plan has none: the recovery point if a session, agent or
// worktree is lost. A plan with `- [ ]`/`- [x]` lines or its own `## Tasks` heading is returned unchanged.
func withTasks(plan []byte) []byte {
	headings := planHeadings(plan)
	if checkboxLine().Match(plan) || hasPlanHeading(headings, templateHeading{level: 2, prefix: "Tasks"}) {
		return plan
	}
	var b bytes.Buffer
	b.Write(bytes.TrimRight(plan, "\n"))
	b.WriteString("\n\n## Tasks\n\nTick `[x]` with the commit id as each item lands; this list is the recovery point if a " +
		"session, agent or worktree is lost.\n\n")
	items := 0
	for _, h := range headings {
		if h.level < 2 || h.level > 4 || !taskHeadingWord().MatchString(h.text) {
			continue
		}
		fmt.Fprintf(&b, "- [ ] %s\n", strings.Trim(h.text, "*_ "))
		items++
	}
	if items == 0 {
		b.WriteString("- [ ] (one line per step of this plan)\n")
	}
	for _, c := range planCloseout {
		fmt.Fprintf(&b, "- [ ] %s\n", c)
	}
	return b.Bytes()
}

var checkboxLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*[-*] \[[ xX]\] `)
})

// undatedNameRe matches a dated plan file's leading YYYY-MM-DD-; anything else directly under plans/ is a
// native plan-mode draft (random words), not yet filed under its repo-tagged, dated name.
var undatedNameRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)
})

// isUndatedDraft reports whether abs is a markdown file sitting directly under <config>/plans/ (not a
// repo-tagged subdirectory) whose name isn't already dated — the shape plan mode's native Write targets.
func isUndatedDraft(abs string) bool {
	if !isMarkdownPath(abs) {
		return false
	}
	// Canonicalize both sides: a draft spelled through a symlink must still match the real plans dir.
	plansDir := core.CanonPath(filepath.Join(paths.ConfigDir(), "plans"))
	if core.CanonPath(filepath.Dir(abs)) != plansDir {
		return false
	}
	return !undatedNameRe().MatchString(filepath.Base(abs))
}

// templateHeading is one section the plan template's requires, matched by level + a text prefix.
type templateHeading struct {
	level  int
	prefix string
	label  string // original "#"-prefixed spelling, for the denial message
}

// PlanTemplateHeadings returns the "#"-prefixed headings the plan template requires.
func PlanTemplateHeadings() []string {
	out := make([]string, len(planTemplateHeadings))
	for i, h := range planTemplateHeadings {
		out[i] = h.label
	}
	return out
}

// planTemplateHeadings are the sections the plan template's requires before a draft is treated as a plan.
var planTemplateHeadings = []templateHeading{
	{1, "Plan:", "# Plan:"},
	{2, "Context", "## Context"},
	{2, "Phases", "## Phases"},
	{2, "Tasks", "## Tasks"},
}

// hasPlanHeading reports whether headings contains one matching want's level and text prefix.
func hasPlanHeading(headings []planHeading, want templateHeading) bool {
	for _, h := range headings {
		if h.level == want.level && strings.HasPrefix(h.text, want.prefix) {
			return true
		}
	}
	return false
}

// Gate denies the FIRST write to an undated draft under <config>/plans/ that lacks the plan
// template's required headings (deny-one-call); the message pastes the skeleton so the retry is right.
// Edit never blocks (it needs an existing file), so only Write, Create and apply_patch Add File matter.
func Gate(in *core.Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool == "apply_patch" { // an Add File hunk is a first write; Update is an Edit, which the gate leaves alone
		for path, content := range patchAdds(applyPatchText(in)) {
			if code := gateDraft(core.AnchorPath(path, in.CWD), content, stderr); code != core.ExitProceed {
				return code
			}
		}
		return core.ExitProceed
	}
	if tool != "" && tool != "write" && tool != "create" {
		return core.ExitProceed
	}
	return gateDraft(core.AnchorPath(in.Nested("tool_input", "file_path"), in.CWD), in.Nested("tool_input", "content"), stderr)
}

// patchAdds yields each file an apply_patch body adds, with its content (the hunk's "+" lines).
func patchAdds(patch string) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		path, lines := "", []string(nil)
		for line := range strings.SplitSeq(patch+"\n*** ", "\n") {
			if !strings.HasPrefix(line, "*** ") {
				if body, ok := strings.CutPrefix(line, "+"); ok && path != "" {
					lines = append(lines, body)
				}
				continue
			}
			if path != "" && !yield(path, strings.Join(lines, "\n")) {
				return
			}
			path, lines = "", nil
			if p, ok := strings.CutPrefix(line, "*** Add File: "); ok {
				path = strings.TrimSpace(p)
			}
		}
	}
}

// gateDraft blocks the first write of content to an undated draft missing a plan-template heading.
func gateDraft(path, content string, stderr io.Writer) int {
	if !isUndatedDraft(path) {
		return core.ExitProceed
	}
	if _, err := os.Stat(path); err == nil {
		return core.ExitProceed // not the first write; the template was already checked
	}
	headings := planHeadings([]byte(content))
	for _, want := range planTemplateHeadings {
		if hasPlanHeading(headings, want) {
			continue
		}
		fmt.Fprintf(stderr, "plan-template: BLOCKED — %s is missing %q from the plan template.\n"+
			"Write the full template from %s before filing a draft:\n%s",
			filepath.Base(path), want.label, plans.TemplatePath(paths.ConfigDir()), skeletonOrHeadings())
		return core.ExitBlock
	}
	return core.ExitProceed
}

// skeletonOrHeadings is the plan template, or the required-heading list when it is unreadable.
func skeletonOrHeadings() string {
	if sk, err := plans.Skeleton(paths.ConfigDir()); err == nil {
		return sk
	}
	var b strings.Builder
	for _, h := range planTemplateHeadings {
		b.WriteString(h.label)
		b.WriteString("\n")
	}
	return b.String()
}

// Mirror is a PostToolUse(Write|Edit) hook: every write to an undated draft under <config>/plans/ is
// copied to its dated, repo-tagged file (Save's own slug/title/dest logic), so the draft is never the
// file of record. The first mirror picks the destination (and never clobbers an unrelated existing dated
// file, via planDestination's collision check); it is then recorded so a later H1 change keeps the same file.
func Mirror(in *core.Input, stderr io.Writer) int {
	path, content, ok := mirrorSource(in)
	if !ok {
		return core.ExitProceed
	}
	err := paths.WithFileLock(filepath.Join(paths.StateDir(), "plan-mirror"), func() error {
		base := filepath.Base(path)
		dest, ok := plans.ReadRecordedDest(base)
		if !ok {
			dest, _ = planDestination(in.CWD, planTitle(content), content, false)
			if err := plans.RecordDest(base, dest); err != nil {
				dropEmptyClaim(dest)
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			dropEmptyClaim(dest)
			return err
		}
		if err := plans.WriteFile(dest, content); err != nil {
			dropEmptyClaim(dest)
			return err
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
	}
	return core.ExitProceed
}

// mirrorSource returns the undated draft a Write/Edit touched and its non-blank content.
func mirrorSource(in *core.Input) (path string, content []byte, ok bool) {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "edit" && tool != "create" {
		return "", nil, false
	}
	path, ok = draftTarget(in)
	if !ok {
		return "", nil, false
	}
	content, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(content)) == 0 {
		return "", nil, false
	}
	return path, content, true
}

func partialFile(session string) string {
	return filepath.Join(plans.ScratchDir(), paths.Slug(session, "", 80, "session")+".partial")
}

func partialsLock() string { return filepath.Join(plans.ScratchDir(), "partials") }

// Reminder warns once at Stop about commits that only partly landed a plan task, then clears the record.
func Reminder(in *core.Input) core.Result {
	var stderr bytes.Buffer
	file := partialFile(in.SessionID)
	if _, err := os.Stat(file); err != nil { // the common Stop: no partial record, so take no lock
		return core.Result{}
	}
	var data []byte
	err := paths.WithFileLock(partialsLock(), func() error {
		var rerr error
		if data, rerr = os.ReadFile(file); rerr != nil {
			return rerr
		}
		if rerr = os.Remove(file); rerr != nil {
			fmt.Fprintf(&stderr, "plan-reminder: %v\n", rerr)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) { // a peer's Stop consumed the record between the Stat and the lock
		return core.Result{}
	}
	if err != nil {
		fmt.Fprintf(&stderr, "plan-reminder: %v\n", err)
		res := core.ExitResult(core.ExitProceed, stderr.String())
		res.SystemMessage = "plans: could not read the partial-task record (" + err.Error() + "); kept for the next Stop"
		return res
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	if items := stillPartial(string(data)); items != "" {
		res.SystemMessage = "plans: this turn's commits only partly landed: " + items + "; finish the task's owns: or tick it by hand"
	}
	return res
}

// stillPartial joins the recorded "<path>\t<plan>: <task>" lines whose task is still open, re-read from the plan.
func stillPartial(data string) string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(data), "\n") {
		path, entry, ok := strings.Cut(line, "\t")
		if !ok {
			path, entry = "", line
		}
		if path != "" {
			if _, task, ok := strings.Cut(entry, ": "); ok && taskDone(path, task) {
				continue
			}
		}
		if entry = strings.Join(strings.Fields(entry), " "); entry != "" {
			out = append(out, entry)
		}
	}
	return strings.Join(out, "; ")
}

// taskDone reports whether the plan at path now shows the titled task ticked.
func taskDone(path, title string) bool {
	p, err := plans.Parse(path)
	if err != nil {
		return false
	}
	for _, t := range p.Tasks {
		if t.Title == title {
			return t.Done
		}
	}
	return false
}
