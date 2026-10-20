package hooks

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
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
	plan = withTasks(plan)

	draftPath := cmp.Or(responsePath, inputPath)
	if !filepath.IsAbs(draftPath) && in.CWD != "" && draftPath != "" {
		draftPath = filepath.Join(in.CWD, draftPath)
	}
	dest, ok := "", false
	if draftPath != "" {
		dest, ok = readRecordedDest(filepath.Base(draftPath))
	}
	fresh := true
	if !ok {
		slug := paths.RepoSlug(cmp.Or(memory.GitToplevel(in.CWD), in.CWD))
		dir := filepath.Join(paths.ConfigDir(), "plans", slug)
		dest, fresh = planDest(dir, time.Now().Format("2006-01-02")+"-"+title, plan)
	}
	if !fresh {
		return ExitProceed
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
		return ExitProceed
	}
	if err := os.WriteFile(dest, plan, 0o600); err != nil {
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
		os.Remove(draftPath)
		removeRecordedDest(filepath.Base(draftPath))
	}
	return ExitProceed
}

// planTitle slugs the first markdown heading, minus a redundant "Plan:" lead-in.
func planTitle(plan []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(plan))
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "#") {
			h := strings.TrimSpace(strings.TrimLeft(line, "# "))
			if strings.HasPrefix(strings.ToLower(h), "plan:") {
				h = h[len("plan:"):]
			}
			return paths.Slug(h, "", 60, "plan")
		}
	}
	return "plan"
}

// planDest picks <base>.md, then <base>-2.md, …; fresh=false when a candidate already holds this exact plan.
func planDest(dir, base string, plan []byte) (string, bool) {
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
		if bytes.Equal(existing, plan) {
			return p, false
		}
	}
}

// taskHeading picks the plan headings that become checklist items: phases, steps, parts and tasks.
var taskHeading = regexp.MustCompile(`(?m)^#{2,4}\s+(.*\b(?:Phase|Step|Part|Task|Stage|Milestone)\b.*?)\s*$`)

// planCloseout is the completion tail every plan's checklist ends with.
var planCloseout = []string{
	"Live check on the real surface",
	"Scoped tests for the touched packages",
	"Review (`claude-code ask --agent codex --fallback vendors`)",
	"Full suite once",
	"Commit + push",
	"Handoff / `/memory:end`",
}

// withTasks appends a `## Tasks` checklist when the plan has none: the recovery point if a session, agent or
// worktree is lost. A plan that already carries `- [ ]`/`- [x]` lines is returned unchanged.
func withTasks(plan []byte) []byte {
	if checkboxLine.Match(plan) {
		return plan
	}
	var b bytes.Buffer
	b.Write(bytes.TrimRight(plan, "\n"))
	b.WriteString("\n\n## Tasks\n\nTick `[x]` with the commit id as each item lands; this list is the recovery point if a " +
		"session, agent or worktree is lost.\n\n")
	items := 0
	for _, m := range taskHeading.FindAllSubmatch(plan, -1) {
		fmt.Fprintf(&b, "- [ ] %s\n", bytes.Trim(m[1], "*_ "))
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

var checkboxLine = regexp.MustCompile(`(?m)^\s*[-*] \[[ xX]\] `)

// undatedNameRe matches a dated plan file's leading YYYY-MM-DD-; anything else directly under plans/ is a
// native plan-mode draft (random words), not yet filed under its repo-tagged, dated name.
var undatedNameRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-`)

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
	return !undatedNameRe.MatchString(filepath.Base(abs))
}

// planTemplateHeadings are the sections agents/Plan.md's template requires before a draft is treated as a plan.
var planTemplateHeadings = []string{"# Plan:", "## Context", "## Phases", "## Tasks"}

// PlanTemplate is a PreToolUse(Write) gate: the FIRST write to an undated draft under <config>/plans/ must
// already carry the plan template's required headings, or it is denied (deny-one-call, like docs-placement).
func PlanTemplate(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	tool := strings.ToLower(in.ToolName)
	if tool != "" && tool != "write" && tool != "create" {
		return ExitProceed
	}
	path := in.nested("tool_input", "file_path")
	if path == "" {
		return ExitProceed
	}
	if !filepath.IsAbs(path) && in.CWD != "" {
		path = filepath.Join(in.CWD, path)
	}
	if !isUndatedDraft(path) {
		return ExitProceed
	}
	if _, err := os.Stat(path); err == nil {
		return ExitProceed // not the first write; the template was already checked
	}
	content := in.nested("tool_input", "content")
	for _, heading := range planTemplateHeadings {
		if strings.Contains(content, heading) {
			continue
		}
		fmt.Fprintf(stderr, "plan-template: BLOCKED — %s is missing %q from the plan template.\n"+
			"Write the full template (# Plan:, ## Context, ## Phases, ## Tasks) from %s before filing a draft.\n",
			filepath.Base(path), heading, filepath.Join(paths.ConfigDir(), "agents", "Plan.md"))
		return ExitBlock
	}
	return ExitProceed
}

// plansScratchDir holds one <draft-basename>.dest record per mirrored draft, pointing at its dated file.
func plansScratchDir() string { return filepath.Join(paths.ConfigDir(), "state", "plans-scratch") }

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

func recordDest(draftBase, dest string) error {
	if err := os.MkdirAll(plansScratchDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destRecordPath(draftBase), []byte(dest), 0o600)
}

func removeRecordedDest(draftBase string) { os.Remove(destRecordPath(draftBase)) }

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
	if !filepath.IsAbs(path) && in.CWD != "" {
		path = filepath.Join(in.CWD, path)
	}
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
		slug := paths.RepoSlug(cmp.Or(memory.GitToplevel(in.CWD), in.CWD))
		dir := filepath.Join(paths.ConfigDir(), "plans", slug)
		title := planTitle(content)
		dest, _ = planDest(dir, time.Now().Format("2006-01-02")+"-"+title, content)
		if err := recordDest(base, dest); err != nil {
			fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
		return ExitProceed
	}
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		fmt.Fprintf(stderr, "plan-mirror: %v\n", err)
	}
	return ExitProceed
}
