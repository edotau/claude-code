package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/launch"
)

// reviewJobEnv names the parent job record a detached split review updates with progress and its outcome.
const reviewJobEnv = "CLAUDE_REVIEW_JOB"

// runSplitReview runs a multi-part review: detached as one job when background, else sequentially in place.
func runSplitReview(vendor string, args []string, parts []agent.Part, scope agent.Scope,
	adversarial, background bool, focus, rubric string) int {
	if background && os.Getenv(reviewJobEnv) == "" {
		return spawnSplitReview(vendor, args, len(parts))
	}
	var job *agent.Record
	if id := os.Getenv(reviewJobEnv); id != "" {
		if r, err := agent.LoadRecord(id); err == nil {
			job = &r
		}
	}
	answers := make([]string, 0, len(parts))
	for i, p := range parts {
		agent.SetReviewProgress(job, fmt.Sprintf("part %d/%d", i+1, len(parts)))
		askArgs, cleanup, err := agent.ReviewArgs(vendor, scope, p.Diff, adversarial, false,
			strings.TrimSpace(focus+"\n"+p.Header(i+1, len(parts))), rubric)
		if err != nil {
			return finishSplitReview(job, fmt.Errorf("part %d/%d: %w", i+1, len(parts), err))
		}
		ans, rc := captureAnswer(func() int { return cmdAsk(askArgs) })
		cleanup()
		if rc != 0 {
			fmt.Print(agent.MergeReports(adversarial, parts[:len(answers)], answers))
			return finishSplitReview(job, fmt.Errorf("part %d/%d failed: %s", i+1, len(parts), agent.LastDispatchError(vendor)))
		}
		answers = append(answers, ans)
	}
	fmt.Print(agent.MergeReports(adversarial, parts, answers))
	return finishSplitReview(job, nil)
}

// spawnSplitReview detaches this same review (minus --background) as one job that runs the parts in order.
func spawnSplitReview(vendor string, args []string, n int) int {
	cwd, _ := os.Getwd()
	req := agent.Request{Prompt: fmt.Sprintf("%s review split into %d parts", vendor, n), WorkDir: cwd, Launcher: launch.Launcher{}}
	rec := agent.BuildAskRecord(context.Background(), vendor, req, nil, cwd, true)
	rec.Progress = fmt.Sprintf("part 0/%d", n)
	_ = rec.Save()
	argv := append([]string{vendor, "review"}, slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		return a == "--background" || a == "-background" || a == "--background=true"
	})...)
	if err := agent.SpawnArgv(&rec, argv, []string{reviewJobEnv + "=" + rec.ID}); err != nil {
		return fail("%s review: %v", vendor, err)
	}
	_ = rec.Save() // record the child's pid so status can tell running from lost
	reportBackgroundStart("text", rec)
	return 0
}

// finishSplitReview closes the parent job record (when any) with err's text and returns the exit code.
func finishSplitReview(job *agent.Record, err error) int {
	if job != nil {
		job.Progress = ""
		job.Finish(agent.Result{}, err, time.Since(job.Time))
		_ = job.Save()
	}
	if err != nil {
		return fail("review: %v", err)
	}
	return 0
}

// captureAnswer runs f with os.Stdout redirected to a pipe and returns what it wrote.
func captureAnswer(f func() int) (string, int) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", f()
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	rc := f()
	os.Stdout = orig
	_ = w.Close()
	return <-done, rc
}
