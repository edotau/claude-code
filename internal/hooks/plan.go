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

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// SavePlan files an approved ExitPlanMode plan as <config>/plans/[<repo>/]<date>-<title>.md (plans/ is gitignored wholesale).
// PostToolUse only fires on approval; tool_input.plan is the approved text, planFilePath its scratch copy.
func SavePlan(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	plan := []byte(in.nested("tool_input", "plan"))
	if len(plan) == 0 {
		plan, _ = os.ReadFile(in.nested("tool_input", "planFilePath"))
	}
	if len(bytes.TrimSpace(plan)) == 0 {
		return ExitProceed
	}
	title := planTitle(plan) // from the approved text, before the checklist adds its own heading
	plan = withTasks(plan)
	slug := paths.RepoSlug(cmp.Or(memory.GitToplevel(in.CWD), in.CWD))
	dir := filepath.Join(paths.ConfigDir(), "plans", slug)
	dest, fresh := planDest(dir, time.Now().Format("2006-01-02")+"-"+title, plan)
	if !fresh {
		return ExitProceed
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
	} else if err := os.WriteFile(dest, plan, 0o600); err != nil {
		fmt.Fprintf(stderr, "save-plan: %v\n", err)
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
