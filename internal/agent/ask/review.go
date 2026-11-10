package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// reviewJobEnv names the parent job record a detached split review updates with progress and its outcome.
const reviewJobEnv = "CLAUDE_REVIEW_JOB"

// reviewDiffTimeout bounds the git calls that build a review diff (a hung textconv helper would block forever).
const reviewDiffTimeout = 2 * time.Minute

// ReviewOptions are `<agent> review`'s flags; Focus is the trailing words.
type ReviewOptions struct {
	Uncommitted, PR, Adversarial, Background bool
	Base, Commit, Rubric, Model, Focus       string
}

// Review is `<agent> review`: review scope's diff in one run, or in parts; a detached split replays args.
func Review(name string, args []string, o ReviewOptions) int {
	scope, err := roles.ParseScope(o.Uncommitted, o.Base, o.Commit, o.PR)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	diff, err := reviewDiff(cwd, scope)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	if parts := roles.ReviewParts(diff, o.Adversarial, o.Focus, o.Rubric); len(parts) > 1 {
		return runSplitReview(name, args, parts, scope, o)
	}
	askArgs, cleanup, err := roles.ReviewArgs(name, scope, diff, o.Adversarial, o.Background, o.Focus, o.Rubric, o.Model)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	defer cleanup()
	return Cmd(askArgs)
}

// reviewDiff materializes scope's diff in cwd under reviewDiffTimeout.
func reviewDiff(cwd string, scope roles.Scope) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reviewDiffTimeout)
	defer cancel()
	return roles.Diff(ctx, cwd, scope)
}

// runSplitReview runs a multi-part review: detached as one job when background, else sequentially in place.
func runSplitReview(name string, args []string, parts []roles.Part, scope roles.Scope, o ReviewOptions) int {
	if o.Background && os.Getenv(reviewJobEnv) == "" {
		return spawnSplitReview(name, args, len(parts))
	}
	var parent *job.Record
	if id := os.Getenv(reviewJobEnv); id != "" {
		r, err := job.LoadRecord(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "review: job record %s not updated: %v\n", id, err)
		} else {
			parent = &r
		}
	}
	answers := make([]string, 0, len(parts))
	for i, p := range parts {
		roles.SetReviewProgress(parent, fmt.Sprintf("part %d/%d", i+1, len(parts)))
		askArgs, cleanup, err := roles.ReviewArgs(name, scope, p.Diff, o.Adversarial, false,
			strings.TrimSpace(o.Focus+"\n"+p.Header(i+1, len(parts))), o.Rubric, o.Model)
		if err != nil {
			return finishSplitReview(parent, fmt.Errorf("part %d/%d: %w", i+1, len(parts), err))
		}
		ans, rc := captureAnswer(func() int { return Cmd(askArgs) })
		cleanup()
		if rc != 0 {
			werr := printMerged(o.Adversarial, parts[:len(answers)], answers)
			return finishSplitReview(parent, errors.Join(
				fmt.Errorf("part %d/%d failed: %s", i+1, len(parts), roles.LastDispatchError(name)), werr))
		}
		answers = append(answers, ans)
	}
	if err := printMerged(o.Adversarial, parts, answers); err != nil {
		return finishSplitReview(parent, fmt.Errorf("write merged report: %w", err))
	}
	return finishSplitReview(parent, nil)
}

// printMerged writes the merged report of the answers so far to stdout and returns any write failure.
func printMerged(adversarial bool, parts []roles.Part, answers []string) error {
	_, err := fmt.Fprint(os.Stdout, roles.MergeReports(adversarial, parts, answers))
	return err
}

// spawnSplitReview detaches this same review (minus --background) as one job that runs the parts in order.
func spawnSplitReview(name string, args []string, n int) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	req := agent.RunOptions{Agent: name, Prompt: fmt.Sprintf("%s review split into %d parts", name, n),
		WorkDir: cwd, Background: true}
	rec, err := NewClient().Record(context.Background(), req, nil)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	rec.Progress = fmt.Sprintf("part 0/%d", n)
	if err := rec.Save(); err != nil { // the child loads this record by id: without it the job would never be visible
		return helptext.Fail("%s review: job record not saved: %v", name, err)
	}
	argv := append([]string{name, "review"}, slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		return a == "--background" || a == "-background" || a == "--background=true"
	})...)
	if err := job.SpawnArgv(&rec, argv, []string{reviewJobEnv + "=" + rec.ID}); err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	if err := rec.Save(); err != nil { // records the child's pid so status can tell running from lost
		fmt.Fprintf(os.Stderr, "%s review: job pid not recorded: %v\n", name, err)
	}
	if err := ReportBackgroundStart("text", rec); err != nil {
		return helptext.Fail("%s review: job %s started; write job id: %v", name, rec.ID, err)
	}
	return 0
}

// finishSplitReview closes the parent job record (when any) with err's text and returns the exit code.
func finishSplitReview(parent *job.Record, err error) int {
	if parent != nil {
		parent.Progress = ""
		parent.Finish(wire.Result{}, err, time.Since(parent.Time))
		if serr := parent.Save(); serr != nil {
			fmt.Fprintf(os.Stderr, "review: job record not saved: %v\n", serr)
		}
	}
	if err != nil {
		return helptext.Fail("review: %v", err)
	}
	return 0
}

// captureAnswer runs f with os.Stdout redirected to a pipe and returns what it wrote.
func captureAnswer(f func() int) (string, int) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", f()
	}
	defer r.Close()
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	rc := f()
	os.Stdout = orig
	_ = w.Close()
	return <-done, rc
}
