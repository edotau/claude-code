package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
	if helptext.Explicit(fs, "format") && fs.Lookup("format").Value.String() != helptext.JSON {
		bad = errors.New("--batch always prints JSON envelopes; drop --format or pass --format json")
	}
	if bad == nil && o.jobs < 1 {
		bad = fmt.Errorf("--jobs %d: need at least 1", o.jobs)
	}
	return bad
}

func runAskBatchInput(fs *flag.FlagSet, src string, o batchOpts, base agent.RunOptions) int {
	in := io.Reader(os.Stdin)
	if src != "-" {
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
	id, ok := batch.Run(ctx, lines, bo, base, helptext.Explicit(fs, "timeout"), newClient, os.Stdout)
	if !ok {
		fmt.Fprintf(os.Stderr, "ask: %s: not every task was ok\n", id)
		return 1
	}
	return 0
}

// batchTask is batch.BuildTask over the cli client.
func batchTask(l batch.ParsedLine, o batchOpts, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (batch.BatchTask, error) {
	return batch.BuildTask(l, batch.Opts{Agent: o.agent, Fallback: o.fallback, Native: o.native}, base, timeoutSet, cwd, seen, newClient)
}
