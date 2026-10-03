package hooks

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// SavePlan files an approved ExitPlanMode plan as <config>/docs/plans/<date>[-<repo>]-<title>.md.
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
	prefix := time.Now().Format("2006-01-02")
	if slug := paths.RepoSlug(cmp.Or(memory.GitToplevel(in.CWD), in.CWD)); slug != "" {
		prefix += "-" + slug
	}
	dir := filepath.Join(paths.ConfigDir(), "docs", "plans")
	dest, fresh := planDest(dir, prefix+"-"+planTitle(plan), plan)
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
