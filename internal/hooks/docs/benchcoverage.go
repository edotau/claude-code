package docs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

type benchTally struct {
	benched, failed, queued, skipped, without, retries int
	regression                                         string
}

// BenchCoverage is the doctor row over top's last n trunk commits; ok is false on a failed run or a regression.
func BenchCoverage(ctx context.Context, top string, n int) (detail string, ok bool) {
	out, err := proc.RunGit(ctx, top, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", n), "HEAD")
	if err != nil {
		return "no git history at " + top, true
	}
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	var t benchTally
	shas := strings.Fields(out)
	benched := benchedCommits(ctx, top, shas)
	for _, sha := range shas {
		if len(benched[sha]) == 0 {
			t.without++
			continue
		}
		t.add(filepath.Join(state, sha))
	}
	detail = fmt.Sprintf("last %d commits: %d benched, %d failed, %d queued, %d skipped, %d without benchmarks",
		len(shas), t.benched, t.failed, t.queued, t.skipped, t.without)
	if t.retries > 0 {
		detail += fmt.Sprintf("; %d await a quiet retry", t.retries)
	}
	if t.regression == "" {
		return detail + "; no regression", t.failed == 0
	}
	return detail + "; latest regression: " + t.regression, false
}

// add counts one benched commit; commits arrive newest first, so the first regression found is the latest.
func (t *benchTally) add(base string) {
	m, ok := benchReadMarker(base)
	switch {
	case ok && m.Exit != nil && *m.Exit != 0:
		t.failed++
	case ok && m.Exit != nil:
		t.benched++
		if m.Retry == "wanted" {
			t.retries++
		}
		if t.regression == "" {
			t.regression = benchRegression(base + ".json")
		}
	case paths.FileExists(base + ".job.json"):
		t.queued++
	default:
		t.skipped++
	}
}

// benchRegression is the result's first judged increase, the same line its notice carried.
func benchRegression(result string) string {
	raw, err := os.ReadFile(result)
	if err != nil {
		return ""
	}
	for _, line := range benchNoticeLines(raw) {
		if benchNoticeRank(line) == 1 {
			return line
		}
	}
	return ""
}
