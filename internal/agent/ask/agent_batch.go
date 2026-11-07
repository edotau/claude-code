package ask

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/batch"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// batchOpts are the ask flags that apply to a whole --batch run.
type batchOpts struct {
	jobs            int
	failFast        bool
	agent, fallback string
	native          bool
	follow          bool
	followIdle      time.Duration
	retryAgents     []string
}

// batchExclusive are the ask flags that make no sense on a batch of independent tasks.
var batchExclusive = []string{"retry", "job", "background", "session", "log", "attach", "rubric", "debug-file"}

// runAskBatch runs the JSONL tasks in src (a file, or "-" for stdin) on a bounded pool and streams one envelope per line.
func runAskBatch(fs *flag.FlagSet, resume bool, src string, o batchOpts, base agent.RunOptions) int {
	if err := checkBatchFlags(fs, resume, o); err != nil {
		return failAskCode("text", o.agent, err, 2)
	}
	return runAskBatchInput(fs, src, o, base)
}

func checkBatchFlags(fs *flag.FlagSet, resume bool, o batchOpts) error {
	var bad error
	if resume || fs.NArg() > 0 {
		bad = errors.New("--batch takes tasks from the JSONL source, not a session id or a positional task")
	}
	for _, f := range batchExclusive {
		if helptext.Explicit(fs, f) {
			bad = fmt.Errorf("--batch cannot be combined with --%s", f)
		}
	}
	if bad == nil {
		bad = checkBatchRun(fs, o)
	}
	return bad
}

// checkBatchRun rejects a non-JSON --format, --follow on stdin and a --jobs below 1.
func checkBatchRun(fs *flag.FlagSet, o batchOpts) error {
	if helptext.Explicit(fs, "format") && fs.Lookup("format").Value.String() != helptext.JSON {
		return errors.New("--batch always prints JSON envelopes; drop --format or pass --format json")
	}
	if o.follow && fs.Lookup("batch").Value.String() == "-" {
		return errors.New("--follow needs a --batch file: stdin cannot be followed")
	}
	if o.jobs < 1 {
		return fmt.Errorf("--jobs %d: need at least 1", o.jobs)
	}
	return nil
}

func runAskBatchInput(fs *flag.FlagSet, src string, o batchOpts, base agent.RunOptions) int {
	in := io.Reader(os.Stdin)
	var fw *follower
	if o.follow {
		var err error
		if fw, in, err = newFollower(src, o.followIdle); err != nil {
			return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
		}
	} else if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
		}
		defer f.Close()
		in = f
	}
	lines, err := batch.ReadTaskLines(in)
	if err != nil {
		return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
	}
	if len(lines) == 0 {
		return failAskCode("text", o.agent, errors.New("--batch: no tasks in the input"), 2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE) // a closed stdout cancels
	defer stop()
	bo := batch.Opts{Jobs: o.jobs, FailFast: o.failFast, Agent: o.agent, Fallback: o.fallback, Native: o.native}
	if as := fs.Lookup("as"); as != nil {
		bo.Identity = as.Value.String()
	}
	bo.Decorate = func(t *batch.Task) { t.Retry = retryFor(*t, bo, o.retryAgents) }
	if fw != nil {
		bo.Follow = fw.run
	}
	id, ok := batch.Run(ctx, lines, bo, base, helptext.Explicit(fs, "timeout"), NewClient, os.Stdout)
	if !ok {
		fmt.Fprintf(os.Stderr, "ask: %s: not every task was ok\n", id)
		return 1
	}
	return 0
}
