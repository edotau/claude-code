package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// reviewJobEnv names the parent job record a detached split review updates with progress and its outcome.
const reviewJobEnv = "CLAUDE_REVIEW_JOB"

// reviewDiffTimeout bounds the git calls that build a review diff (a hung textconv helper would block forever).
var reviewDiffTimeout = 2 * time.Minute

// ReviewOptions are `<agent> review`'s flags; Focus is the trailing words.
type ReviewOptions struct {
	Uncommitted, PR, Adversarial, Background   bool
	Base, Commit, Rubric, Model, Focus, Format string
}

// Review is `<agent> review`: review scope's diff in one run, or in parts; a detached split replays args.
func Review(name string, args []string, o ReviewOptions) int {
	parent, perr := reviewParent()
	if perr != nil { // the job record cannot be closed later, so do not run a review nobody will be told about
		return helptext.Fail("%s review: %v", name, perr)
	}
	started := time.Now()
	fail := func(err error) int {
		if parent != nil { // a detached split's child: every exit closes the job record
			return finishSplitReview(parent, err)
		}
		return helptext.Fail("%s review: %v", name, err)
	}
	scope, err := parseScope(o.Uncommitted, o.Base, o.Commit, o.PR)
	if err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	diff, o, err := reviewInput(name, cwd, scope, o)
	if err != nil {
		return fail(err)
	}
	parts := reviewParts(diff, o.Adversarial, o.Focus, o.Rubric)
	if o.Background && o.Format == "json" && parent == nil {
		return spawnSplitReview(name, args, len(parts), o.Format)
	}
	if len(parts) > 1 {
		return runSplitReview(name, args, parts, o, parent)
	}
	return runSingleReview(name, diff, o, parent, started)
}

func reviewInput(name, cwd string, scope diffScope, o ReviewOptions) ([]byte, ReviewOptions, error) {
	diff, err := reviewDiff(cwd, scope)
	if err != nil {
		return nil, o, err
	}
	o, err = reviewModelOptions(name, o)
	return diff, o, err
}

func reviewModelOptions(name string, o ReviewOptions) (ReviewOptions, error) {
	if name != "copilot" {
		return o, nil
	}
	model, err := copilot.ReviewModel(context.Background(), o.Model, reviewAuthorModel())
	o.Model = model
	return o, err
}

func reviewAuthorModel() string {
	if id := os.Getenv(proc.DispatchIDEnv); id != "" {
		if record, err := agent.LoadRecord(id); err == nil && record.Model != "" {
			return record.Model
		}
	}
	return os.Getenv("CLAUDE_MODEL")
}

func runSingleReview(name string, diff []byte, o ReviewOptions, parent *agent.Record, started time.Time) int {
	askArgs, cleanup, err := reviewArgs(name, diff, o.Adversarial, o.Background, o.Focus, o.Rubric, o.Model)
	if err != nil {
		if parent != nil {
			return finishSplitReview(parent, err)
		}
		return helptext.Fail("%s review: %v", name, err)
	}
	defer cleanup()
	if parent == nil {
		return runReviewAsk(name, askArgs, o)
	}
	ans, usage, rc := reviewPartAnswer(name, askArgs, o) // the parent record sums the one run's usage
	addReviewUsage(parent, usage)
	if _, err := io.WriteString(os.Stdout, ans); err != nil {
		return finishSplitReview(parent, fmt.Errorf("write review: %w", err))
	}
	if rc != 0 { // the diff no longer splits in the child: the one run's outcome is the job's
		return finishSplitReview(parent, fmt.Errorf("review failed: %s", lastDispatchError(name, started)))
	}
	return finishSplitReview(parent, nil)
}

// reviewParent loads the job record a detached split review's child reports to; nil in a foreground review.
func reviewParent() (*agent.Record, error) {
	id := os.Getenv(reviewJobEnv)
	if id == "" {
		return nil, nil
	}
	r, err := agent.LoadRecord(id)
	if err != nil {
		return nil, fmt.Errorf("job record %s unreadable: %w", id, err)
	}
	return &r, nil
}

// reviewDiff materializes scope's diff in cwd under reviewDiffTimeout.
func reviewDiff(cwd string, scope diffScope) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reviewDiffTimeout)
	defer cancel()
	return diff(ctx, cwd, scope)
}

// runSplitReview runs a multi-part review: detached as one job when background, else sequentially in place.
func runSplitReview(name string, args []string, parts []part, o ReviewOptions, parent *agent.Record) int {
	if o.Background && os.Getenv(reviewJobEnv) == "" {
		return spawnSplitReview(name, args, len(parts), o.Format)
	}
	answers := make([]string, 0, len(parts))
	for i, p := range parts {
		setReviewProgress(parent, fmt.Sprintf("part %d/%d", i+1, len(parts)))
		askArgs, cleanup, err := reviewArgs(name, p.Diff, o.Adversarial, false,
			strings.TrimSpace(o.Focus+"\n"+p.Header(i+1, len(parts))), o.Rubric, o.Model)
		if err != nil {
			werr := printReviewMerged(o, parts, answers)
			return finishSplitReview(parent, errors.Join(fmt.Errorf("part %d/%d: %w", i+1, len(parts), err), werr))
		}
		partStart := time.Now()
		ans, usage, rc := reviewPartAnswer(name, askArgs, o)
		addReviewUsage(parent, usage)
		cleanup()
		if rc != 0 {
			werr := printReviewMerged(o, parts, answers)
			return finishSplitReview(parent, errors.Join(
				fmt.Errorf("part %d/%d failed: %s", i+1, len(parts), lastDispatchError(name, partStart)), werr))
		}
		answers = append(answers, ans)
	}
	if err := printReviewMerged(o, parts, answers); err != nil {
		return finishSplitReview(parent, fmt.Errorf("write merged report: %w", err))
	}
	return finishSplitReview(parent, nil)
}

// printMerged writes the merged report of the answers so far to stdout and returns any write failure.
func printMerged(adversarial bool, parts []part, answers []string) error {
	_, err := fmt.Fprint(os.Stdout, mergeReports(adversarial, parts, answers))
	return err
}

// spawnSplitReview detaches this same review (minus --background) as one job that runs the parts in order.
func spawnSplitReview(name string, args []string, n int, format string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	req := agent.RunOptions{Agent: name, Prompt: fmt.Sprintf("%s review split into %d parts", name, n),
		WorkDir: cwd, Background: true}
	ctx := context.Background()
	rec, err := newClient().Record(ctx, req, nil)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	rec.Progress = fmt.Sprintf("part 0/%d", n)
	if err := rec.Save(); err != nil { // the child loads this record by id: without it the job would never be visible
		return helptext.Fail("%s review: job record not saved: %v", name, err)
	}
	argv := append([]string{name, "review"}, slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		return isBackgroundFlag(a)
	})...)
	if err := agent.SpawnArgvContext(ctx, &rec, argv, []string{reviewJobEnv + "=" + rec.ID}); err != nil {
		return helptext.Fail("%s review: %v", name, failSpawn(&rec, err))
	}
	if err := rec.Save(); err != nil { // records the child's pid so status can tell running from lost
		fmt.Fprintf(os.Stderr, "%s review: job pid not recorded: %v\n", name, err)
	}
	if err := reportBackgroundStart(format, rec); err != nil {
		return helptext.Fail("%s review: job %s started; write job id: %v", name, rec.ID, err)
	}
	return 0
}

// isBackgroundFlag matches --background and -background, bare or with =value.
func isBackgroundFlag(a string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
	return strings.HasPrefix(a, "-") && name == "background"
}

// finishSplitReview closes the parent job record (when any) with err's text and returns the exit code.
func finishSplitReview(parent *agent.Record, err error) int {
	if parent != nil {
		parent.Progress = ""
		result := wire.Result{}
		if parent.Usage != nil {
			result.Usage = *parent.Usage
		}
		parent.Finish(result, err, time.Since(parent.Time))
		if serr := parent.Save(); serr != nil {
			fmt.Fprintf(os.Stderr, "review: job record not saved: %v\n", serr)
		}
	}
	if err != nil {
		return helptext.Fail("review: %v", err)
	}
	return 0
}

// captureAnswer runs f with a private writer for its answer and returns what it wrote; stdout is untouched.
func captureAnswer(f func(out io.Writer) int) (string, int) {
	var b bytes.Buffer
	rc := f(&b)
	return b.String(), rc
}

func sumReviewUsage(a, b wire.Usage) wire.Usage {
	return wire.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens, CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens,
		CostUSD: a.CostUSD + b.CostUSD}
}

func addReviewUsage(parent *agent.Record, usage wire.Usage) {
	if parent == nil {
		return
	}
	total := usage
	if parent.Usage != nil {
		total = sumReviewUsage(*parent.Usage, usage)
	}
	parent.Usage = &total
}
