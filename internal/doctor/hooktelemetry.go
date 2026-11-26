package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooktelemetry"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

func hookTelemetryChecks(now time.Time) []check {
	report := hooktelemetry.Read(now, hooktelemetry.Activity)
	modules := report.Modules()
	detail := "module | fires | denies | overruns | p95 ms"
	if len(modules) == 0 {
		detail = "no runs recorded"
	}
	if report.Probes > 0 {
		detail += fmt.Sprintf(" (+%d probe rows skipped)", report.Probes)
	}
	if report.Dropped > 0 {
		detail += fmt.Sprintf("; incomplete (%d cap markers)", report.Dropped)
	}
	if report.Truncated {
		detail += "; read limit reached"
	}
	rows := []check{{Name: "hook telemetry (7 days, since " + report.Start + ")", OK: true, Detail: detail}}
	for _, m := range modules {
		p95 := "-"
		if m.HasDuration {
			p95 = fmt.Sprintf("%.2f", m.P95)
		}
		rows = append(rows, check{Name: "hook " + m.Name, OK: true,
			Detail: fmt.Sprintf("%d | %d | %d | %s", m.Fires, m.Denies, m.Overruns, p95)})
	}
	return rows
}

const benchCoverageCommits = 12

// benchCoverageCheck is the post-commit bench row: the R22 trigger's reach; a failure or regression is a soft ✗.
func benchCoverageCheck(ctx context.Context) check {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	detail, ok := docs.BenchCoverage(ctx, paths.ConfigDir(), benchCoverageCommits)
	return check{Name: "post-commit bench", OK: ok, Detail: detail}
}

// fixNowCheck is the soft ✗ for failures whose fix-now directive went unclaimed DirectiveMax times.
func fixNowCheck(ctx context.Context) check {
	open, err := worktree.UnclaimedDirectives(ctx, worktree.FailuresDir())
	if err != nil {
		return check{Name: "failure directives", Detail: "failure log unreadable: " + err.Error()}
	}
	if len(open) == 0 {
		return check{Name: "failure directives", OK: true, Detail: "none unclaimed"}
	}
	f := open[0]
	return check{Name: "failure directives", Detail: fmt.Sprintf("%d unclaimed after %d deliveries; newest %s %s: claude-code failures claim %s",
		len(open), worktree.DirectiveMax, f.Kind, f.Subject, f.ID)}
}
