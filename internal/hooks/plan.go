package hooks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/markdown"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// SavePlan files an approved ExitPlanMode plan as <config>/plans/[<repo>/]<date>-<title>.md (plans/ is gitignored wholesale).
// PostToolUse only fires on approval; prefer approved input text, then the response, then their scratch files.
// When the approved plan traces back to an undated draft the plan-mirror hook already filed, its recorded
// destination is reused (the date stays the draft's creation date) instead of minting a new one for today.
func SavePlan(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	plan := []byte(in.nested("tool_input", "plan"))
	if len(plan) == 0 {
		plan = []byte(in.nested("tool_response", "plan"))
	}
	responsePath := in.nested("tool_response", "filePath")
	inputPath := in.nested("tool_input", "planFilePath")
	for _, path := range []string{responsePath, inputPath} {
		if len(plan) != 0 {
			break
		}
		if path != "" {
			var err error
			plan, err = os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(stderr, "save-plan: %v\n", err)
			}
		}
	}
	if len(bytes.TrimSpace(plan)) == 0 {
		return ExitProceed
	}
	title := planTitle(plan) // from the approved text, before the checklist adds its own heading
	approvedText := plan     // snapshot before withTasks, to compare against the draft's current bytes below
	plan = withTasks(plan)

	draftPath := resolveCWDPath(cmp.Or(responsePath, inputPath), in.CWD)
	dest, ok := "", false
	if draftPath != "" {
		dest, ok = readRecordedDest(filepath.Base(draftPath))
	}
	fresh := true
	if !ok {
		dest, fresh = planDestination(in.CWD, title, plan, true)
	}
	if !fresh {
		return ExitProceed
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
		return ExitProceed
	}
	if err := plans.WriteFile(dest, plan); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
		return ExitProceed
	}
	// Verify the dated file actually holds the plan before touching the draft it was mirrored from.
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, plan) {
		fmt.Fprintf(stderr, "save-plan: verification failed for %s\n", dest)
		return ExitProceed
	}
	audit.Log(audit.PlanSaved, "save-plan", dest)
	if draftPath != "" && isUndatedDraft(draftPath) {
		// Only delete the draft when its current bytes still match what was approved — an edit made to the
		// draft after approval (e.g. by a concurrent agent) must be kept, not silently discarded.
		switch current, err := os.ReadFile(draftPath); {
		case os.IsNotExist(err):
			// already gone; nothing to clean up
		case err != nil:
			fmt.Fprintf(stderr, "save-plan: %v\n", err)
		case !bytes.Equal(current, approvedText):
			fmt.Fprintf(stderr, "save-plan: draft %s was edited after approval; keeping it\n", draftPath)
		default:
			if err := os.Remove(draftPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "save-plan: %v\n", err)
				return ExitProceed
			}
			if err := os.Remove(destRecordPath(filepath.Base(draftPath))); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "save-plan: %v\n", err)
			}
		}
	}
	return ExitProceed
}

// planHeading is one heading's level and display text, as parsed from a plan's AST — a "#" inside a
// fenced code block or YAML front matter never produces one, unlike a raw line/substring scan.
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
var planSlugLine = regexp.MustCompile("(?im)^(?:suggested )?slug:\\s*`?([a-z0-9][a-z0-9-]*)`?\\s*$")

// planTitle prefers an explicit slug line, else slugs the first heading minus a redundant "Plan:" lead-in.
func planTitle(plan []byte) string {
	if m := planSlugLine.FindSubmatch(plan); m != nil {
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

// resolveCWDPath anchors path to cwd when it isn't already absolute.
func resolveCWDPath(path, cwd string) string {
	if path != "" && !filepath.IsAbs(path) && cwd != "" {
		return filepath.Join(cwd, path)
	}
	return path
}

// planDestination picks <date>-<title>.md, then -2.md, … under the repo's plans dir; adopt reuses a
// candidate already holding content (fresh=false) — save-plan adopts, a mirror never takes a file it did not create.
func planDestination(cwd, title string, content []byte, adopt bool) (dest string, fresh bool) {
	dir := plans.Home(cmp.Or(memory.GitToplevel(cwd), cwd))
	base := time.Now().Format("2006-01-02") + "-" + title
	for n := 1; ; n++ {
		name := base + ".md"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.md", base, n)
		}
		p := filepath.Join(dir, name)
		existing, err := os.ReadFile(p)
		if err != nil {
			return p, true
		}
		if adopt && bytes.Equal(existing, content) {
			return p, false
		}
	}
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
	"Commit + push",
	"Handoff / `claude-code memory update`",
}

// withTasks appends a `## Tasks` checklist when the plan has none: the recovery point if a session, agent or
// worktree is lost. A plan that already carries `- [ ]`/`- [x]` lines is returned unchanged.
func withTasks(plan []byte) []byte {
	if checkboxLine().Match(plan) {
		return plan
	}
	var b bytes.Buffer
	b.Write(bytes.TrimRight(plan, "\n"))
	b.WriteString("\n\n## Tasks\n\nTick `[x]` with the commit id as each item lands; this list is the recovery point if a " +
		"session, agent or worktree is lost.\n\n")
	items := 0
	for _, h := range planHeadings(plan) {
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
	plansDir := filepath.Join(paths.ConfigDir(), "plans")
	if filepath.Dir(abs) != filepath.Clean(plansDir) {
		return false
	}
	return !undatedNameRe().MatchString(filepath.Base(abs))
}

// templateHeading is one section agents/Plan.md's template requires, matched by level + a text prefix.
type templateHeading struct {
	level  int
	prefix string
	label  string // original "#"-prefixed spelling, for the denial message
}

// planTemplateHeadings are the sections agents/Plan.md's template requires before a draft is treated as a plan.
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

// PlanGate is the PreToolUse(Write|Edit) plan-gate hook: the first write to a native plan draft must carry the template.
func PlanGate(r io.Reader, stderr io.Writer) int { return checkPlanTemplate(ParseInput(r), stderr) }

// checkPlanTemplate denies the FIRST write to an undated draft under <config>/plans/ that lacks the plan
// template's required headings (deny-one-call); the message pastes the skeleton so the retry is right.
func checkPlanTemplate(in *Input, stderr io.Writer) int {
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "create" {
		return ExitProceed
	}
	path := in.nested("tool_input", "file_path")
	if path == "" {
		return ExitProceed
	}
	path = resolveCWDPath(path, in.CWD)
	if !isUndatedDraft(path) {
		return ExitProceed
	}
	if _, err := os.Stat(path); err == nil {
		return ExitProceed // not the first write; the template was already checked
	}
	content := in.nested("tool_input", "content")
	headings := planHeadings([]byte(content))
	for _, want := range planTemplateHeadings {
		if hasPlanHeading(headings, want) {
			continue
		}
		fmt.Fprintf(stderr, "plan-template: BLOCKED — %s is missing %q from the plan template.\n"+
			"Write the full template from %s before filing a draft:\n%s",
			filepath.Base(path), want.label, filepath.Join(paths.ConfigDir(), "agents", "Plan.md"), skeletonOrHeadings())
		return ExitBlock
	}
	return ExitProceed
}

// skeletonOrHeadings is the Plan.md skeleton, or the required-heading list when Plan.md is unreadable.
func skeletonOrHeadings() string {
	if sk, err := plans.Skeleton(paths.ConfigDir()); err == nil {
		return sk
	}
	var b strings.Builder
	for _, h := range planTemplateHeadings {
		b.WriteString(h.label + "\n")
	}
	return b.String()
}

// plansScratchDir holds one <draft-basename>.dest record per mirrored draft, pointing at its dated file.
func plansScratchDir() string { return filepath.Join(paths.StateDir(), "plans-scratch") }

func destRecordPath(draftBase string) string {
	return filepath.Join(plansScratchDir(), draftBase+".dest")
}

func readRecordedDest(draftBase string) (string, bool) {
	data, err := os.ReadFile(destRecordPath(draftBase))
	if err != nil {
		return "", false
	}
	dest := strings.TrimSpace(string(data))
	return dest, dest != ""
}

// MirrorDests lists every unapproved draft's dated mirror, so no tick ever lands on a plan still being drafted.
func MirrorDests() []string {
	recs, _ := filepath.Glob(filepath.Join(plansScratchDir(), "*.dest"))
	var out []string
	for _, r := range recs {
		if dest, ok := readRecordedDest(strings.TrimSuffix(filepath.Base(r), ".dest")); ok {
			out = append(out, dest)
		}
	}
	return out
}

func recordDest(draftBase, dest string) error {
	if err := os.MkdirAll(plansScratchDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destRecordPath(draftBase), []byte(dest), 0o600)
}

// PlanMirror is a PostToolUse(Write|Edit) hook: every write to an undated draft under <config>/plans/ is
// copied to its dated, repo-tagged file (SavePlan's own slug/title/dest logic), so the draft is never the
// file of record. The first mirror picks the destination (and never clobbers an unrelated existing dated
// file, via planDest's collision check); it is then recorded so a later H1 change keeps the same file.
func PlanMirror(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "edit" && tool != "create" {
		return ExitProceed
	}
	path := in.nested("tool_input", "file_path")
	if path == "" {
		return ExitProceed
	}
	path = resolveCWDPath(path, in.CWD)
	if !isUndatedDraft(path) {
		return ExitProceed
	}
	content, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(content)) == 0 {
		return ExitProceed
	}
	base := filepath.Base(path)
	dest, ok := readRecordedDest(base)
	if !ok {
		dest, _ = planDestination(in.CWD, planTitle(content), content, false)
		if err := recordDest(base, dest); err != nil {
			fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
		return ExitProceed
	}
	if err := plans.WriteFile(dest, content); err != nil {
		fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
	}
	return ExitProceed
}

func skeletonMarker(session string) string {
	return filepath.Join(plansScratchDir(), paths.Slug(session, "", 80, "session")+".skeleton")
}

func partialFile(session string) string {
	return filepath.Join(plansScratchDir(), paths.Slug(session, "", 80, "session")+".partial")
}

// PlanSkeleton injects the Plan.md skeleton on EnterPlanMode and, once per plan, on a plan-mode prompt.
func PlanSkeleton(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	event := in.HookEventName
	marker := skeletonMarker(in.SessionID)
	if event == "UserPromptSubmit" {
		if in.nested("permission_mode") != "plan" {
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "plan-skeleton: %v\n", err)
			}
			return ExitProceed
		}
		if _, err := os.Stat(marker); err == nil {
			return ExitProceed
		}
	} else if in.ToolName != "EnterPlanMode" {
		return ExitProceed
	}
	sk, err := plans.Skeleton(paths.ConfigDir())
	if err != nil {
		return ExitProceed // no Plan.md: fail open
	}
	err = os.MkdirAll(plansScratchDir(), 0o700)
	if err == nil {
		err = os.WriteFile(marker, nil, 0o600)
	}
	if err != nil {
		fmt.Fprintf(stderr, "plan-skeleton: %v\n", err)
	}
	fmt.Fprint(stdout, MergeContext(event, []string{"Plan format (agents/Plan.md) — this format replaces plan mode's " +
		"suggested headings:\n" + sk}, in.Vendor()))
	return ExitProceed
}

// PlanReminder warns once at Stop about commits that only partly landed a plan task, then clears the record.
func PlanReminder(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	file := partialFile(in.SessionID)
	data, err := os.ReadFile(file)
	if err != nil {
		return ExitProceed
	}
	if err := os.Remove(file); err != nil {
		fmt.Fprintf(stderr, "plan-reminder: %v\n", err)
	}
	items := stillPartial(string(data))
	if items == "" {
		return ExitProceed
	}
	msg := "plans: this turn's commits only partly landed: " + items + "; finish the task's owns: or tick it by hand"
	out, _ := json.Marshal(map[string]string{"systemMessage": msg})
	fmt.Fprint(stdout, string(out))
	return ExitProceed
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
